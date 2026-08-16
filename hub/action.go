package hub

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/That1Drifter/vyshka/hub/internal/id"
	"github.com/That1Drifter/vyshka/hub/internal/schema"
	"github.com/That1Drifter/vyshka/hub/store"
)

// Envelope types the action lifecycle uses (spec section 7).
const (
	envelopeTypeActionDispatch = "action.dispatch"
	envelopeTypeActionAck      = "action.ack"
	envelopeTypeActionResult   = "action.result"
)

// Action limits.
const (
	// DefaultActionTTL applies when a dispatch requests nothing; the bounds
	// clamp what it may request (spec section 7).
	DefaultActionTTL = 120 * time.Second
	MinActionTTL     = time.Second
	MaxActionTTL     = 24 * time.Hour

	// maxActionResultBytes caps the result payload of one action, measured on
	// the raw bytes of the serialized value (section 7).
	maxActionResultBytes = 64 << 10
	// maxActionErrorBytes caps the error string the same way; without it, the
	// result cap could be laundered through a 900 KiB "error".
	maxActionErrorBytes = 4 << 10

	maxReferenceKeyLength   = 200
	maxIdempotencyKeyLength = 200
)

// clampActionTTL applies the section 7 bounds to a requested TTL. Absent,
// zero, or negative means the default. The comparison happens in seconds, not
// time.Duration, because a huge request multiplied by time.Second would wrap
// negative and sail under the cap it was meant to hit.
func clampActionTTL(requestedSeconds int) time.Duration {
	if requestedSeconds <= 0 {
		return DefaultActionTTL
	}
	if requestedSeconds > int(MaxActionTTL/time.Second) {
		return MaxActionTTL
	}
	return time.Duration(requestedSeconds) * time.Second
}

type dispatchRequest struct {
	Code           string          `json:"code"`
	Context        string          `json:"context"`
	ReferenceKey   string          `json:"referenceKey"`
	Params         json.RawMessage `json:"params"`
	TTLSeconds     int             `json:"ttlSeconds"`
	IdempotencyKey string          `json:"idempotencyKey"`
}

// actionDispatchBody is the body of the action.dispatch envelope: what the
// plugin needs to execute and report, including the deadline it must discard
// the action after (spec section 7).
type actionDispatchBody struct {
	ActionID     string          `json:"actionId"`
	Code         string          `json:"code"`
	Context      string          `json:"context,omitempty"`
	ReferenceKey string          `json:"referenceKey,omitempty"`
	Params       json.RawMessage `json:"params"`
	ExpiresAt    string          `json:"expiresAt"`
}

// handleDispatchAction validates a dispatch against the server's manifest and
// queues it (spec section 7). Validation happens here, before anything is
// stored: schema-invalid input never reaches the game server (section 6.1).
func (s *Server) handleDispatchAction(w http.ResponseWriter, r *http.Request) {
	server, ok := s.lookupServer(w, r)
	if !ok {
		return
	}

	var request dispatchRequest
	if !s.decodeJSON(w, r, &request) {
		return
	}

	switch {
	case request.Code == "":
		writeError(w, http.StatusBadRequest, codeBadRequest, "code is required")
		return
	case utf8.RuneCountInString(request.Code) > maxCodeLength:
		writeError(w, http.StatusBadRequest, codeBadRequest, "code is too long")
		return
	case utf8.RuneCountInString(request.ReferenceKey) > maxReferenceKeyLength:
		writeError(w, http.StatusBadRequest, codeBadRequest, "referenceKey is too long")
		return
	case utf8.RuneCountInString(request.IdempotencyKey) > maxIdempotencyKeyLength:
		writeError(w, http.StatusBadRequest, codeBadRequest, "idempotencyKey is too long")
		return
	}

	// The idempotency contract comes before everything else that could refuse
	// this request (spec section 7): a retry of an action that was already
	// accepted returns the original even if the manifest has since changed in
	// a way that would refuse a fresh dispatch of the same body.
	if request.IdempotencyKey != "" {
		original, err := s.store.ActionByIdempotencyKey(r.Context(), server.ID, request.IdempotencyKey)
		switch {
		case err == nil:
			writeJSON(w, http.StatusAccepted, map[string]any{
				"actionId": original.ID,
				"state":    original.State,
			})
			return
		case !errors.Is(err, store.ErrNotFound):
			s.writeInternalError(w, r, err)
			return
		}
	}

	params := json.RawMessage(`{}`)
	if len(request.Params) > 0 && string(request.Params) != "null" {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(request.Params, &object); err != nil {
			writeError(w, http.StatusBadRequest, codeBadRequest, "params must be a JSON object")
			return
		}
		params = request.Params
	}

	// Validate against the stored manifest, then dispatch gated on the
	// revision the validation used. A republish landing in between refuses the
	// insert (ErrManifestChanged), and this loop revalidates against the new
	// manifest rather than queue params it never approved. Two rounds settle
	// any realistic race; a manifest that changes faster than that is answered
	// with conflict rather than chased forever.
	for attempt := 0; ; attempt++ {
		revision, ok := s.validateDispatch(w, r, server.ID, request.Code, params)
		if !ok {
			return
		}

		actionID := id.NewAt(time.Now().UTC())
		expiresAt := time.Now().UTC().Add(clampActionTTL(request.TTLSeconds)).Truncate(time.Millisecond)
		envelopeBody, err := json.Marshal(actionDispatchBody{
			ActionID:     actionID,
			Code:         request.Code,
			Context:      request.Context,
			ReferenceKey: request.ReferenceKey,
			Params:       params,
			ExpiresAt:    envelopeTimestamp(expiresAt),
		})
		if err != nil {
			s.writeInternalError(w, r, err)
			return
		}

		action, created, err := s.store.DispatchAction(r.Context(), store.NewAction{
			ID:               actionID,
			ServerID:         server.ID,
			Code:             request.Code,
			Context:          request.Context,
			ReferenceKey:     request.ReferenceKey,
			Params:           params,
			IdempotencyKey:   request.IdempotencyKey,
			ExpiresAt:        expiresAt,
			ManifestRevision: revision,
			EnvelopeType:     envelopeTypeActionDispatch,
			EnvelopeBody:     envelopeBody,
			QueueLimit:       outboundQueueLimit,
		})
		switch {
		case errors.Is(err, store.ErrManifestChanged):
			if attempt < 2 {
				continue
			}
			writeError(w, http.StatusConflict, codeConflict,
				"the server's manifest kept changing while this dispatch was being validated; try again")
			return
		case errors.Is(err, store.ErrOutboundQueueFull):
			writeError(w, http.StatusConflict, codeOutboundQueueFull,
				"this server already has "+strconv.Itoa(outboundQueueLimit)+" unacked envelopes queued")
			return
		case errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "no such server")
			return
		case err != nil:
			s.writeInternalError(w, r, err)
			return
		}

		if created {
			s.waiters.notify(server.ID)
			s.log.Info("action dispatched",
				"serverId", server.ID, "actionId", action.ID, "code", action.Code)
		}
		writeJSON(w, http.StatusAccepted, map[string]any{
			"actionId": action.ID,
			"state":    action.State,
		})
		return
	}
}

// validateDispatch checks a dispatch against the server's stored manifest and
// returns the revision the check used, answering the client itself when the
// dispatch cannot proceed.
func (s *Server) validateDispatch(w http.ResponseWriter, r *http.Request, serverID, code string, params json.RawMessage) (int64, bool) {
	manifest, err := s.store.Manifest(r.Context(), serverID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusConflict, codeUnknownAction,
			"this server has not published a manifest, so no action can be validated or dispatched")
		return 0, false
	case err != nil:
		s.writeInternalError(w, r, err)
		return 0, false
	}

	var body manifestBody
	if err := json.Unmarshal(manifest.Body, &body); err != nil {
		s.writeInternalError(w, r, err)
		return 0, false
	}
	var declared *manifestAction
	for i := range body.Actions {
		if body.Actions[i].Code == code {
			declared = &body.Actions[i]
			break
		}
	}
	if declared == nil {
		writeError(w, http.StatusConflict, codeUnknownAction,
			"the server's manifest (revision "+strconv.FormatInt(manifest.Revision, 10)+
				") does not declare the action "+code)
		return 0, false
	}

	if len(declared.Params) > 0 && string(declared.Params) != "null" {
		compiled, faults := schema.Compile(declared.Params)
		if len(faults) > 0 {
			// The manifest validator refuses these at publish; a stored schema
			// that no longer compiles is a hub bug, not a client error.
			s.writeInternalError(w, r, errors.New("stored manifest schema failed to compile: "+faults[0].String()))
			return 0, false
		}
		if faults := compiled.Validate(params); len(faults) > 0 {
			details := make([]map[string]string, 0, len(faults))
			for _, fault := range faults {
				details = append(details, map[string]string{"path": fault.Path, "message": fault.Message})
			}
			writeErrorDetails(w, http.StatusBadRequest, codeParamsInvalid,
				"params do not satisfy the schema this action declared",
				map[string]any{"errors": details})
			return 0, false
		}
	}
	return manifest.Revision, true
}

// actionView is the Admin API representation of an action record.
type actionView struct {
	ID             string          `json:"id"`
	ServerID       string          `json:"serverId"`
	Code           string          `json:"code"`
	Context        string          `json:"context,omitempty"`
	ReferenceKey   string          `json:"referenceKey,omitempty"`
	Params         json.RawMessage `json:"params"`
	IdempotencyKey string          `json:"idempotencyKey,omitempty"`
	State          string          `json:"state"`
	CreatedAt      time.Time       `json:"createdAt"`
	ExpiresAt      time.Time       `json:"expiresAt"`
	DeliveredAt    *time.Time      `json:"deliveredAt"`
	RunningAt      *time.Time      `json:"runningAt"`
	FinishedAt     *time.Time      `json:"finishedAt"`
	OK             *bool           `json:"ok"`
	Result         json.RawMessage `json:"result"`
	Error          *string         `json:"error"`
	DurationMs     *int64          `json:"durationMs"`
}

func newActionView(action store.Action) actionView {
	view := actionView{
		ID:             action.ID,
		ServerID:       action.ServerID,
		Code:           action.Code,
		Context:        action.Context,
		ReferenceKey:   action.ReferenceKey,
		Params:         action.Params,
		IdempotencyKey: action.IdempotencyKey,
		State:          action.State,
		CreatedAt:      action.CreatedAt,
		ExpiresAt:      action.ExpiresAt,
		DeliveredAt:    action.DeliveredAt,
		RunningAt:      action.RunningAt,
		FinishedAt:     action.FinishedAt,
		OK:             action.OK,
		Result:         action.Result,
		DurationMs:     action.DurationMs,
	}
	if action.Error != "" {
		view.Error = &action.Error
	}
	return view
}

// handleGetAction answers with one action record (spec section 7).
func (s *Server) handleGetAction(w http.ResponseWriter, r *http.Request) {
	action, err := s.store.ActionByID(r.Context(), r.PathValue("actionId"))
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, codeNotFound, "no such action")
		return
	case err != nil:
		s.writeInternalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, newActionView(action))
}

// preparedAction is one action.ack or action.result envelope after body
// parsing: at most one of the fields is set, and both nil means the body was
// unusable and is ignored (acked and logged, never fatal to the batch).
type preparedAction struct {
	ack    string
	result *store.ActionResult
}

// prepareActions parses every action.ack and action.result body in a batch,
// keyed by batch index. Like manifests, parsing runs before classification
// because it depends only on content.
//
// A body the hub cannot use is not an envelope error: the envelope is still
// acked (its durable effect is nothing), because failing the batch would stall
// every later envelope behind a message the plugin cannot fix by retrying.
func prepareActions(envelopes []inboundEnvelope) map[int]preparedAction {
	var prepared map[int]preparedAction
	for index, e := range envelopes {
		if e.Type != envelopeTypeActionAck && e.Type != envelopeTypeActionResult {
			continue
		}
		if prepared == nil {
			prepared = make(map[int]preparedAction)
		}

		if e.Type == envelopeTypeActionAck {
			var body struct {
				ActionID string `json:"actionId"`
			}
			if err := json.Unmarshal(e.Body, &body); err != nil || body.ActionID == "" {
				prepared[index] = preparedAction{}
				continue
			}
			prepared[index] = preparedAction{ack: body.ActionID}
			continue
		}

		var body struct {
			ActionID   string          `json:"actionId"`
			OK         *bool           `json:"ok"`
			Result     json.RawMessage `json:"result"`
			Error      *string         `json:"error"`
			DurationMs *int64          `json:"durationMs"`
		}
		if err := json.Unmarshal(e.Body, &body); err != nil || body.ActionID == "" || body.OK == nil {
			prepared[index] = preparedAction{}
			continue
		}
		result := store.ActionResult{
			ActionID:   body.ActionID,
			OK:         *body.OK,
			Result:     body.Result,
			DurationMs: body.DurationMs,
		}
		if string(result.Result) == "null" {
			result.Result = nil
		}
		if result.DurationMs != nil && *result.DurationMs < 0 {
			// A negative duration is nonsense, not an error: dropped like any
			// other advisory field the hub cannot use.
			result.DurationMs = nil
		}
		if body.Error != nil {
			result.Error = *body.Error
		}
		// The 64 KiB cap of section 7, and its smaller sibling on error. The
		// outcome is recorded either way; an over-cap payload is dropped and
		// says so, because storing it would let one action monopolize the
		// database and rejecting the envelope would strand the batch behind
		// it.
		if len(result.Error) > maxActionErrorBytes {
			result.Error = truncateUTF8(result.Error, maxActionErrorBytes) + " [truncated]"
		}
		if len(result.Result) > maxActionResultBytes {
			result.Result = nil
			result.Error = joinNonEmpty(result.Error,
				"the result payload exceeded the 64 KiB cap and was dropped")
		}
		prepared[index] = preparedAction{result: &result}
	}
	return prepared
}

func joinNonEmpty(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}

// truncateUTF8 cuts a string to at most limit bytes without splitting a rune.
func truncateUTF8(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
