package hub

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/That1Drifter/vyshka/hub/internal/id"
	"github.com/That1Drifter/vyshka/hub/internal/token"
	"github.com/That1Drifter/vyshka/hub/store"
)

// Webhook limits (spec section 11). The counts bound what one registration can
// make every dispatcher pass evaluate; the URL cap is the usual "one field must
// not be a novel" rule.
const (
	maxWebhookURLLength     = 2048
	maxWebhookEventFilters  = 20
	maxWebhookServerIDs     = 50
	maxWebhookRedactPaths   = 20
	maxRedactPathLength     = 128
	webhookDeliveryPageSize = 100
	maxWebhookDeliveryPage  = 500
	// pendingDeliveryBound is the per-webhook pending queue bound of section
	// 11.5: at the bound a new delivery is dead on arrival, visibly.
	pendingDeliveryBound = 1000

	templateGenericJSON = "generic-json"
)

type createWebhookRequest struct {
	URL       string   `json:"url"`
	Events    []string `json:"events"`
	ServerIDs []string `json:"serverIds"`
	Template  string   `json:"template"`
	Redact    []string `json:"redact"`
}

// updateWebhookRequest is the section 11.2 edit: every member is optional and
// a pointer, because absent and empty mean different things here. An absent
// member leaves the field alone; a present one replaces it whole, so
// `"events": []` subscribes to every type exactly as it does at registration.
type updateWebhookRequest struct {
	URL       *string   `json:"url"`
	Events    *[]string `json:"events"`
	ServerIDs *[]string `json:"serverIds"`
	Template  *string   `json:"template"`
	Redact    *[]string `json:"redact"`
	Paused    *bool     `json:"paused"`
}

// webhookView is a webhook as the Admin API reports it: everything but the
// secret, which left the hub exactly once, in the registration response.
type webhookView struct {
	ID        string     `json:"id"`
	URL       string     `json:"url"`
	Events    []string   `json:"events"`
	ServerIDs []string   `json:"serverIds"`
	Template  string     `json:"template"`
	Redact    []string   `json:"redact"`
	CreatedAt time.Time  `json:"createdAt"`
	PausedAt  *time.Time `json:"pausedAt"`
}

func newWebhookView(webhook store.Webhook) webhookView {
	view := webhookView{
		ID:        webhook.ID,
		URL:       webhook.URL,
		Events:    webhook.Events,
		ServerIDs: webhook.ServerIDs,
		Template:  webhook.Template,
		Redact:    webhook.Redact,
		CreatedAt: webhook.CreatedAt,
		PausedAt:  webhook.PausedAt,
	}
	if view.Redact == nil {
		view.Redact = []string{}
	}
	if view.Events == nil {
		view.Events = []string{}
	}
	if view.ServerIDs == nil {
		view.ServerIDs = []string{}
	}
	return view
}

// handleCreateWebhook registers a webhook (spec section 11.2). The signing
// secret is minted here and returned here, and never again.
func (s *Server) handleCreateWebhook(w http.ResponseWriter, r *http.Request) {
	var request createWebhookRequest
	if !s.decodeJSON(w, r, &request) {
		return
	}

	target, parsed, ok := validateWebhookURL(w, request.URL)
	if !ok {
		return
	}
	events, ok := validateWebhookEvents(w, request.Events)
	if !ok {
		return
	}

	// A webhook is a standing export of everything its filter matches, so the
	// registering token must hold grants covering the subscription, or
	// webhooks:manage would quietly be an installation-wide read grant (spec
	// section 11.2).
	if !s.requireSubscriptionCoverage(w, r, events, len(request.ServerIDs) > 0) {
		return
	}

	serverIDs, ok := s.validateWebhookServerIDs(w, r, request.ServerIDs)
	if !ok {
		return
	}
	template, ok := validateWebhookTemplate(w, request.Template)
	if !ok {
		return
	}
	redact, ok := validateWebhookRedact(w, request.Redact)
	if !ok {
		return
	}

	webhook, err := s.store.CreateWebhook(r.Context(), store.Webhook{
		ID:        id.New(),
		URL:       target,
		Secret:    token.New(token.Webhook),
		Template:  template,
		Events:    events,
		ServerIDs: serverIDs,
		Redact:    redact,
		// The coverage check above has already refused a filter admitting
		// the audit notification to anything but admin.
		AuditGranted: filterAdmitsAudit(events),
	})
	if err != nil {
		s.writeInternalError(w, r, err)
		return
	}

	// The audited and logged URL is redacted: target URLs routinely embed
	// bearer credentials in their query or path userinfo, and logs outlive and
	// outtravel webhook configuration (spec section 11.2).
	auditDetail(r, "webhookId", webhook.ID)
	auditDetail(r, "url", redactURL(parsed))
	s.log.Info("webhook registered",
		"webhookId", webhook.ID, "url", redactURL(parsed),
		"events", len(events), "servers", len(serverIDs))
	writeJSON(w, http.StatusCreated, map[string]any{
		"webhook": newWebhookView(webhook),
		"secret":  webhook.Secret,
	})
}

// The registration validators of section 11.2, shared by POST and PATCH so an
// edit can never be a second, laxer spelling of the same rules. Each answers
// the client itself and reports false when the value is refused.

// validateWebhookURL checks the target URL and hands back both the trimmed
// value to store and the parsed form the audit trail redacts.
func validateWebhookURL(w http.ResponseWriter, value string) (string, *url.URL, bool) {
	target := strings.TrimSpace(value)
	switch {
	case target == "":
		writeError(w, http.StatusBadRequest, codeBadRequest, "url is required")
		return "", nil, false
	case len(target) > maxWebhookURLLength:
		writeError(w, http.StatusBadRequest, codeBadRequest, "url is too long")
		return "", nil, false
	}
	parsed, err := url.Parse(target)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		writeError(w, http.StatusBadRequest, codeBadRequest, "url must be http or https")
		return "", nil, false
	}
	return target, parsed, true
}

// validateWebhookEvents checks the filter against the section 10.1 grammar.
func validateWebhookEvents(w http.ResponseWriter, values []string) ([]string, bool) {
	if len(values) > maxWebhookEventFilters {
		writeError(w, http.StatusBadRequest, codeBadRequest,
			"a webhook subscribes with at most "+strconv.Itoa(maxWebhookEventFilters)+" event patterns")
		return nil, false
	}
	events := make([]string, 0, len(values))
	for _, value := range values {
		pattern := strings.TrimSpace(value)
		// The section 10.1 grammar, verbatim: a pattern outside it must be
		// refused rather than become a filter that silently matches nothing.
		if pattern != "*" && !validScopePattern(pattern) {
			writeError(w, http.StatusBadRequest, codeBadRequest,
				"event pattern "+pattern+" is not *, a {namespace}.* prefix, or an exact type")
			return nil, false
		}
		events = append(events, pattern)
	}
	return events, true
}

// validateWebhookServerIDs checks that every named server exists.
func (s *Server) validateWebhookServerIDs(w http.ResponseWriter, r *http.Request, values []string) ([]string, bool) {
	if len(values) > maxWebhookServerIDs {
		writeError(w, http.StatusBadRequest, codeBadRequest,
			"a webhook observes at most "+strconv.Itoa(maxWebhookServerIDs)+" named servers; leave serverIds empty to observe every server")
		return nil, false
	}
	serverIDs := make([]string, 0, len(values))
	for _, value := range values {
		serverID := strings.TrimSpace(value)
		// A typo here would otherwise become a webhook that never fires, which
		// looks exactly like a hub that never delivers (spec section 11.2).
		if _, err := s.store.Server(r.Context(), serverID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeError(w, http.StatusNotFound, codeNotFound, "no such server: "+serverID)
				return nil, false
			}
			s.writeInternalError(w, r, err)
			return nil, false
		}
		serverIDs = append(serverIDs, serverID)
	}
	return serverIDs, true
}

// validateWebhookTemplate resolves the template, defaulting to generic-json.
func validateWebhookTemplate(w http.ResponseWriter, value string) (string, bool) {
	template := strings.TrimSpace(value)
	if template == "" {
		template = templateGenericJSON
	}
	if template != templateGenericJSON && template != templateDiscord {
		writeError(w, http.StatusBadRequest, codeBadRequest, unknownTemplateMessage(template))
		return "", false
	}
	return template, true
}

// validateWebhookRedact checks the redaction paths of section 11.2: member
// names from the identifier alphabet joined by dots, at most twenty of them.
// A path outside the grammar is refused rather than kept as one that could
// never strip anything, which would look exactly like a redaction that works.
func validateWebhookRedact(w http.ResponseWriter, values []string) ([]string, bool) {
	if len(values) > maxWebhookRedactPaths {
		writeError(w, http.StatusBadRequest, codeBadRequest,
			"a webhook redacts at most "+strconv.Itoa(maxWebhookRedactPaths)+" paths")
		return nil, false
	}
	paths := make([]string, 0, len(values))
	for _, value := range values {
		path := strings.TrimSpace(value)
		if !validRedactPath(path) {
			writeError(w, http.StatusBadRequest, codeBadRequest,
				"redact path "+truncateUTF8(path, 64)+" is not member names of letters, digits, _ and - joined by dots, at most "+
					strconv.Itoa(maxRedactPathLength)+" characters")
			return nil, false
		}
		paths = append(paths, path)
	}
	return paths, true
}

func validRedactPath(path string) bool {
	if path == "" || len(path) > maxRedactPathLength {
		return false
	}
	for _, segment := range strings.Split(path, ".") {
		if segment == "" {
			return false
		}
		for i := range len(segment) {
			c := segment[i]
			switch {
			case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
			default:
				return false
			}
		}
	}
	return true
}

// handleUpdateWebhook edits one registration in place (spec section 11.2). An
// absent member is left alone and a present one replaces its field whole; the
// coverage rule is then re-applied to the **resulting** subscription, so an
// edit can never widen a webhook past what the editing token could have
// registered. The secret is neither rotated nor returned.
func (s *Server) handleUpdateWebhook(w http.ResponseWriter, r *http.Request) {
	var request updateWebhookRequest
	if !s.decodeJSON(w, r, &request) {
		return
	}

	// A body carrying nothing this draft recognises is a request the caller
	// cannot have meant; answering 200 to it would report success for an edit
	// that never happened. It is refused before the webhook is read, so a
	// malformed edit reads the same whatever id it names.
	if len(updatedWebhookFields(request)) == 0 {
		writeError(w, http.StatusBadRequest, codeBadRequest,
			"an edit names at least one of url, events, serverIds, template, redact, paused")
		return
	}

	webhookID := r.PathValue("webhookId")
	existing, err := s.store.WebhookByID(r.Context(), webhookID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, codeNotFound, "no such webhook")
		return
	case err != nil:
		s.writeInternalError(w, r, err)
		return
	}

	update := store.WebhookUpdate{Paused: request.Paused}
	// Every edit re-authorizes the whole subscription, so every edit decides
	// the audit grant afresh, from the filter as merged on the locked row:
	// an edit that passed coverage with a filter admitting the audit
	// notification was made by a token that reads the audit log.
	update.AuditGranted = func(existing store.Webhook) bool {
		if update.Events != nil {
			return filterAdmitsAudit(*update.Events)
		}
		return filterAdmitsAudit(existing.Events)
	}

	var parsedURL *url.URL
	if request.URL != nil {
		target, parsed, ok := validateWebhookURL(w, *request.URL)
		if !ok {
			return
		}
		update.URL, parsedURL = &target, parsed
	}
	if request.Events != nil {
		events, ok := validateWebhookEvents(w, *request.Events)
		if !ok {
			return
		}
		update.Events = &events
	}

	// The coverage decision, taken over the webhook as it stands rather than
	// over the members that changed: the merged subscription is what the
	// token must have been able to register, and, when the URL moves, so is
	// every delivery still pending, because those bodies were rendered under
	// the old subscription and will follow the URL to wherever the editor
	// points it. The decision is a closure because it is taken twice: once
	// here on a snapshot, so that a refusal precedes the server lookups the
	// enforcement order of section 10.2 puts after it, and once more inside
	// the store's transaction on the locked row, where a concurrent edit can
	// no longer have changed what was authorized.
	caller := principalFrom(r.Context())
	authorize := func(current store.Webhook, pendingTypes []string) error {
		mergedEvents := current.Events
		if update.Events != nil {
			mergedEvents = *update.Events
		}
		mergedServerIDs := current.ServerIDs
		if request.ServerIDs != nil {
			mergedServerIDs = *request.ServerIDs
		}
		if refused := subscriptionCoverage(caller, mergedEvents, len(mergedServerIDs) > 0); refused != nil {
			return refused
		}
		for _, notificationType := range pendingTypes {
			if refused := notificationCoverage(caller, notificationType); refused != nil {
				return refused
			}
		}
		return nil
	}
	var pendingTypes []string
	if update.URL != nil && *update.URL != existing.URL {
		// Only a URL that actually moves carries the pending bodies
		// anywhere; an edit that repeats the current one is not a retarget.
		if pendingTypes, err = s.store.PendingDeliveryTypes(r.Context(), webhookID); err != nil {
			s.writeInternalError(w, r, err)
			return
		}
	}
	if err := authorize(existing, pendingTypes); err != nil {
		writeRefusal(w, err)
		return
	}

	if request.ServerIDs != nil {
		serverIDs, ok := s.validateWebhookServerIDs(w, r, *request.ServerIDs)
		if !ok {
			return
		}
		update.ServerIDs = &serverIDs
	}
	if request.Template != nil {
		template, ok := validateWebhookTemplate(w, *request.Template)
		if !ok {
			return
		}
		update.Template = &template
	}
	if request.Redact != nil {
		redact, ok := validateWebhookRedact(w, *request.Redact)
		if !ok {
			return
		}
		update.Redact = &redact
	}

	webhook, err := s.store.UpdateWebhook(r.Context(), webhookID, update, authorize)
	var refused *refusal
	switch {
	case errors.As(err, &refused):
		// The row changed under the snapshot and the token does not cover
		// what it became: the edit lost the race and is refused as it would
		// have been had it arrived second.
		writeRefusal(w, err)
		return
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, codeNotFound, "no such webhook")
		return
	case err != nil:
		s.writeInternalError(w, r, err)
		return
	}

	auditDetail(r, "webhookId", webhook.ID)
	auditDetail(r, "fields", updatedWebhookFields(request))
	if parsedURL != nil {
		// Redacted for the same reason registration redacts it: target URLs
		// routinely carry credentials past the host (spec section 11.2).
		auditDetail(r, "url", redactURL(parsedURL))
	}
	if request.Paused != nil {
		auditDetail(r, "paused", *request.Paused)
	}
	s.log.Info("webhook edited",
		"webhookId", webhook.ID, "fields", updatedWebhookFields(request),
		"paused", webhook.PausedAt != nil)
	if request.Paused != nil && !*request.Paused {
		// Everything the pause held back is due now; the dispatcher need not
		// wait for its next tick to find out.
		s.nudgeWebhooks()
	}
	writeJSON(w, http.StatusOK, map[string]any{"webhook": newWebhookView(webhook)})
}

// writeRefusal answers a coverage decision taken as an error. Anything that
// is not a refusal is a programming error on the way here, and is answered as
// forbidden rather than leaked as an internal failure.
func writeRefusal(w http.ResponseWriter, err error) {
	var refused *refusal
	if errors.As(err, &refused) {
		writeError(w, refused.status, refused.code, refused.message)
		return
	}
	writeError(w, http.StatusForbidden, codeForbidden, err.Error())
}

// updatedWebhookFields names the members one edit carried, for the audit
// record and the log. The values themselves stay out of both: a filter is
// noise there, and a URL is a credential.
func updatedWebhookFields(request updateWebhookRequest) []string {
	fields := make([]string, 0, 6)
	if request.URL != nil {
		fields = append(fields, "url")
	}
	if request.Events != nil {
		fields = append(fields, "events")
	}
	if request.ServerIDs != nil {
		fields = append(fields, "serverIds")
	}
	if request.Template != nil {
		fields = append(fields, "template")
	}
	if request.Redact != nil {
		fields = append(fields, "redact")
	}
	if request.Paused != nil {
		fields = append(fields, "paused")
	}
	return fields
}

// handleReplayWebhookDelivery re-arms one delivery for a further attempt (spec
// section 11.5). The delivery keeps its id, its stored body, and its attempt
// count, so what the receiver sees is the same signed bytes it was owed, not a
// new delivery of an old notification.
func (s *Server) handleReplayWebhookDelivery(w http.ResponseWriter, r *http.Request) {
	webhookID, deliveryID := r.PathValue("webhookId"), r.PathValue("deliveryId")
	notFound := func() {
		// One code for an unknown webhook and for a delivery belonging to
		// another one: neither is a delivery this webhook can replay.
		writeError(w, http.StatusNotFound, codeNotFound, "no such delivery on this webhook")
	}
	stored, err := s.store.WebhookDelivery(r.Context(), webhookID, deliveryID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		notFound()
		return
	case err != nil:
		s.writeInternalError(w, r, err)
		return
	}
	// A replay sends a rendered body again, so the token must cover its
	// type as it would have to cover a filter of that type: a delivery is an
	// export of what it carries, whatever the webhook subscribes to now. The
	// type never changes, so the check needs no lock.
	if refused := notificationCoverage(principalFrom(r.Context()), stored.Type); refused != nil {
		writeRefusal(w, refused)
		return
	}

	delivery, err := s.store.ReplayWebhookDelivery(r.Context(), webhookID, deliveryID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		notFound()
		return
	case err != nil:
		s.writeInternalError(w, r, err)
		return
	}

	auditDetail(r, "webhookId", webhookID)
	auditDetail(r, "deliveryId", deliveryID)
	s.log.Info("webhook delivery replayed",
		"webhookId", webhookID, "deliveryId", deliveryID,
		"type", delivery.Type, "attempts", delivery.Attempts)
	s.nudgeWebhooks()
	writeJSON(w, http.StatusAccepted, map[string]any{"delivery": newDeliveryView(delivery)})
}

// handleListWebhooks answers with every webhook, newest first, secrets omitted.
func (s *Server) handleListWebhooks(w http.ResponseWriter, r *http.Request) {
	webhooks, err := s.store.Webhooks(r.Context())
	if err != nil {
		s.writeInternalError(w, r, err)
		return
	}
	views := make([]webhookView, 0, len(webhooks))
	for _, webhook := range webhooks {
		views = append(views, newWebhookView(webhook))
	}
	writeJSON(w, http.StatusOK, map[string]any{"webhooks": views})
}

// handleDeleteWebhook removes a webhook and abandons its pending deliveries.
func (s *Server) handleDeleteWebhook(w http.ResponseWriter, r *http.Request) {
	webhookID := r.PathValue("webhookId")
	switch err := s.store.DeleteWebhook(r.Context(), webhookID); {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, codeNotFound, "no such webhook")
		return
	case err != nil:
		s.writeInternalError(w, r, err)
		return
	}
	auditDetail(r, "webhookId", webhookID)
	s.log.Info("webhook deleted", "webhookId", webhookID)
	w.WriteHeader(http.StatusNoContent)
}

// deliveryView is one delivery in the section 11.5 record. DeliveredAt is a
// pointer without omitempty so an undelivered row reports null rather than
// hiding the field, and NextAttemptAt appears only while it means something.
type deliveryView struct {
	ID            string     `json:"id"`
	Type          string     `json:"type"`
	ServerID      string     `json:"serverId"`
	State         string     `json:"state"`
	Attempts      int        `json:"attempts"`
	LastStatus    *int       `json:"lastStatus,omitempty"`
	LastError     string     `json:"lastError,omitempty"`
	CreatedAt     time.Time  `json:"createdAt"`
	NextAttemptAt *time.Time `json:"nextAttemptAt,omitempty"`
	DeliveredAt   *time.Time `json:"deliveredAt"`
}

// handleListWebhookDeliveries exposes a webhook's delivery record, newest
// first: the visible counter section 11.5 requires before anything may fail.
func (s *Server) handleListWebhookDeliveries(w http.ResponseWriter, r *http.Request) {
	webhook, err := s.store.WebhookByID(r.Context(), r.PathValue("webhookId"))
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, codeNotFound, "no such webhook")
		return
	case err != nil:
		s.writeInternalError(w, r, err)
		return
	}

	// The page bound is the caller's to widen, clamped rather than refused,
	// because "dead remains readable" (section 11.5) has to survive a burst of
	// newer records.
	limit, ok := parseLimitParam(w, r.URL.Query().Get("limit"), webhookDeliveryPageSize, maxWebhookDeliveryPage)
	if !ok {
		return
	}
	deliveries, err := s.store.WebhookDeliveries(r.Context(), webhook.ID, limit)
	if err != nil {
		s.writeInternalError(w, r, err)
		return
	}

	views := make([]deliveryView, 0, len(deliveries))
	for _, delivery := range deliveries {
		views = append(views, newDeliveryView(delivery))
	}
	writeJSON(w, http.StatusOK, map[string]any{"deliveries": views})
}

func newDeliveryView(delivery store.WebhookDelivery) deliveryView {
	view := deliveryView{
		ID:          delivery.ID,
		Type:        delivery.Type,
		ServerID:    delivery.ServerID,
		State:       delivery.State,
		Attempts:    delivery.Attempts,
		LastStatus:  delivery.LastStatus,
		LastError:   delivery.LastError,
		CreatedAt:   delivery.CreatedAt,
		DeliveredAt: delivery.DeliveredAt,
	}
	if delivery.State == store.DeliveryPending {
		nextAttempt := delivery.NextAttemptAt
		view.NextAttemptAt = &nextAttempt
	}
	return view
}

// redactURL renders a webhook URL safe for logs and the audit trail: scheme
// and host only. Credentials live in queries, fragments, userinfo, and, on
// services like chat platforms, in the path itself, so everything after the
// host goes. The full URL stays readable where it belongs, on the webhook
// record behind webhooks:manage.
func redactURL(parsed *url.URL) string {
	return parsed.Scheme + "://" + parsed.Host
}

// reservedNamespace reports whether a type, or a pattern, lies in a namespace
// the hub's own notifications own (spec section 8.1): action, server, and
// audit. Telemetry may not use them, which is what keeps a plugin from
// speaking in the hub's voice to every webhook receiver.
func reservedNamespace(value string) bool {
	return strings.HasPrefix(value, "action.") || strings.HasPrefix(value, "server.") ||
		strings.HasPrefix(value, "audit.")
}

// optInNotification reports whether a notification type is matched only by a
// pattern that names its namespace, never by the catch-all (spec section
// 11.1). The audit log is admin-only, and a subscription to everything
// registered before the notification existed must not start exporting it.
func optInNotification(notificationType string) bool {
	return strings.HasPrefix(notificationType, "audit.")
}

// patternAdmits reports whether one webhook filter pattern matches one
// notification type, with the opt-in rule applied.
func patternAdmits(pattern, notificationType string) bool {
	if pattern == "*" && optInNotification(notificationType) {
		return false
	}
	return (Scope{Pattern: pattern}).matches(notificationType)
}

// filterAdmitsAudit reports whether a webhook filter matches the opt-in audit
// notification. An empty filter is the catch-all, which never does.
func filterAdmitsAudit(events []string) bool {
	for _, pattern := range events {
		if patternAdmits(pattern, notifyAuditRecorded) {
			return true
		}
	}
	return false
}

// subscribesTelemetry reports whether a pattern can match any telemetry type.
// The section 8.1 reservation is what makes this decidable: a pattern
// confined to the reserved namespaces can only ever match the hub's own
// notifications.
func subscribesTelemetry(pattern string) bool {
	return pattern == "*" || !reservedNamespace(pattern)
}

// refusal is a coverage decision the handler answers with, carried as an
// error so it can be taken inside a store transaction and told apart from a
// store failure on the way out.
type refusal struct {
	status  int
	code    string
	message string
}

func (r *refusal) Error() string { return r.code + ": " + r.message }

func forbidden(message string) *refusal {
	return &refusal{status: http.StatusForbidden, code: codeForbidden, message: message}
}

// requireSubscriptionCoverage enforces the section 11.2 registration rule: the
// caller's grants must cover everything the filter subscribes to. It answers
// the client itself and returns false when they do not.
func (s *Server) requireSubscriptionCoverage(w http.ResponseWriter, r *http.Request, events []string, namesServers bool) bool {
	if refused := subscriptionCoverage(principalFrom(r.Context()), events, namesServers); refused != nil {
		writeError(w, refused.status, refused.code, refused.message)
		return false
	}
	return true
}

// subscriptionCoverage decides the section 11.2 rule for one subscription
// without touching the response, so the same decision can be taken again
// inside the edit transaction.
func subscriptionCoverage(caller *principal, events []string, namesServers bool) *refusal {
	subscribed := events
	if len(subscribed) == 0 {
		subscribed = []string{"*"}
	}
	needsActions, needsServers, needsAdmin := false, namesServers, false
	for _, pattern := range subscribed {
		if patternAdmits(pattern, notifyActionCompleted) {
			// The notification carries any code's record, so nothing narrower
			// than an unnarrowed actions:read can cover it.
			needsActions = true
		}
		if patternAdmits(pattern, notifyServerLinkLost) || patternAdmits(pattern, notifyServerLinkRestore) {
			needsServers = true
		}
		if patternAdmits(pattern, notifyAuditRecorded) {
			// The audit log is read under admin alone (spec section 10.5), and
			// an export of it is a reading of it.
			needsAdmin = true
		}
		if subscribesTelemetry(pattern) {
			if !caller.covers(resourceEvents, verbRead, pattern) {
				return forbidden("subscribing to " + pattern + " requires a scope covering events:read:" + pattern +
					"; a webhook is a standing export of what it matches")
			}
		}
	}
	if needsActions && !caller.covers(resourceActions, verbRead, "*") {
		return forbidden("subscribing to action.completed requires actions:read; the notification carries any action's record")
	}
	if needsServers && !caller.allowsAny(resourceServers, verbRead) {
		return forbidden("subscribing to the link notifications, or naming serverIds, requires servers:read")
	}
	if needsAdmin && !caller.isAdmin() {
		return forbidden("subscribing to audit.recorded requires admin, the scope that reads the audit log")
	}
	return nil
}

// notificationCoverage decides whether the caller's grants cover one stored
// notification type: the subscription rule applied to a filter of exactly that
// type. It gates what an edit that retargets a webhook may carry along (the
// pending deliveries) and what a replay may send again, because a delivery
// already rendered is an export of that type wherever the URL now points.
func notificationCoverage(caller *principal, notificationType string) *refusal {
	switch {
	case notificationType == notifyActionCompleted:
		if !caller.covers(resourceActions, verbRead, "*") {
			return forbidden("a pending action.completed delivery requires actions:read to be redirected or replayed")
		}
	case notificationType == notifyServerLinkLost || notificationType == notifyServerLinkRestore:
		if !caller.allowsAny(resourceServers, verbRead) {
			return forbidden("a pending " + notificationType + " delivery requires servers:read to be redirected or replayed")
		}
	case optInNotification(notificationType):
		if !caller.isAdmin() {
			return forbidden("a delivery of " + notificationType + " requires admin to be redirected or replayed")
		}
	default:
		if !caller.covers(resourceEvents, verbRead, notificationType) {
			return forbidden("a delivery of " + notificationType + " requires a scope covering events:read:" +
				notificationType + " to be redirected or replayed")
		}
	}
	return nil
}

// webhookMatches decides whether one notification concerns one webhook: the
// server filter is exact membership, the event filter the section 10.1 pattern
// grammar, and empty filters mean everything (spec section 11.2).
func webhookMatches(webhook store.Webhook, notificationType, serverID string) bool {
	if len(webhook.ServerIDs) > 0 {
		observed := false
		for _, candidate := range webhook.ServerIDs {
			if candidate == serverID {
				observed = true
				break
			}
		}
		if !observed {
			return false
		}
	}
	if optInNotification(notificationType) && !webhook.AuditGranted {
		// A filter naming the audit namespace that no token able to read the
		// audit log ever authorized, one registered while the namespace was
		// ordinary telemetry above all, is not a request for the access
		// record (spec section 11.1).
		return false
	}
	if len(webhook.Events) == 0 {
		// An empty filter is the catch-all, and the catch-all never admits an
		// opt-in notification (spec section 11.1).
		return !optInNotification(notificationType)
	}
	for _, pattern := range webhook.Events {
		if patternAdmits(pattern, notificationType) {
			return true
		}
	}
	return false
}
