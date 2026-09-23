package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// Checks for the hub features of protocol draft 0.31: the player profile
// (spec sections 8.2 and 8.6), per-webhook redaction (section 11.2), and audit
// records as webhook material (sections 11.1 and 10.5), with the reservation
// of the audit namespace that makes the last one decidable (section 8.1).

// uniqueIdentity mints a platform id no other run of the suite has used, so a
// profile check against a long-lived hub reads only its own events.
func uniqueIdentity() string {
	return "conformance-" + strconv.FormatInt(time.Now().UnixNano(), 36) + "-" +
		strconv.FormatInt(unknownTypeCounter.Add(1), 10)
}

func steamIdentity(id string) map[string]any {
	return map[string]any{"platform": "steam", "id": id}
}

type playerEventRecord struct {
	eventRecord
	Roles []string `json:"roles"`
}

type playerEventPage struct {
	Events     []playerEventRecord `json:"events"`
	NextCursor string              `json:"nextCursor"`
}

func playerPath(platform, playerID, tail string) string {
	return "/api/v1/players/" + url.PathEscape(platform) + "/" + url.PathEscape(playerID) + tail
}

func (e Env) playerEvents(ctx context.Context, bearer, playerID string, parameters url.Values) (playerEventPage, error) {
	path := playerPath("steam", playerID, "/events")
	if encoded := parameters.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var page playerEventPage
	err := e.expect(ctx, http.MethodGet, path, bearer, nil, http.StatusOK, &page)
	return page, err
}

func checkReservedNamespaces(ctx context.Context, env Env) error {
	plugin, err := env.newFakePlugin(ctx, "conformance: reserved namespaces", shortPollTimeoutSeconds)
	if err != nil {
		return err
	}
	serverID := plugin.Server.Server.ID
	for _, reserved := range []string{"action.completed", "server.link.lost", "audit.recorded"} {
		batch := plugin.nextOutbound("event.batch", eventBatch(map[string]any{"t": reserved}))
		response, err := plugin.send(ctx, batch)
		if err != nil {
			return fmt.Errorf("a batch in the reserved %s namespace failed the poll; refusal is envelope-level success: %w",
				strings.SplitN(reserved, ".", 2)[0], err)
		}
		if response.Ack < batch.Seq {
			return fmt.Errorf("ack = %d after a refused %s batch, want at least %d", response.Ack, reserved, batch.Seq)
		}
	}
	page, err := env.events(ctx, serverID, nil)
	if err != nil {
		return err
	}
	if len(page.Events) != 0 {
		return fmt.Errorf("the feed holds %v; the action, server, and audit namespaces are the hub's own, "+
			"and telemetry in them must be refused (section 8.1)", page.eventTypes())
	}
	return nil
}

func checkPlayerEvents(ctx context.Context, env Env) error {
	first, err := env.newFakePlugin(ctx, "conformance: profile one", shortPollTimeoutSeconds)
	if err != nil {
		return err
	}
	second, err := env.newFakePlugin(ctx, "conformance: profile two", shortPollTimeoutSeconds)
	if err != nil {
		return err
	}
	subject, other := uniqueIdentity(), uniqueIdentity()
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	at := func(minutes int) string { return base.Add(time.Duration(minutes) * time.Minute).Format(time.RFC3339) }

	if _, err := first.sendEvents(ctx,
		map[string]any{"t": "core.player.connect", "ts": at(1), "data": map[string]any{"player": steamIdentity(subject)}},
		map[string]any{"t": "core.player.death", "ts": at(2), "data": map[string]any{
			"player": steamIdentity(other), "killer": steamIdentity(subject),
		}},
		// Not references: nested, a member name in the wrong case, an empty id.
		map[string]any{"t": "core.vehicle.destroy", "ts": at(3), "data": map[string]any{
			"crew": []any{map[string]any{"player": steamIdentity(subject)}},
		}},
		map[string]any{"t": "conformance-profile.misses", "ts": at(4), "data": map[string]any{
			"a": map[string]any{"Platform": "steam", "id": subject},
			"b": map[string]any{"platform": "steam", "ID": subject},
		}},
	); err != nil {
		return err
	}
	if _, err := second.sendEvents(ctx,
		map[string]any{"t": "core.player.death", "ts": at(5), "data": map[string]any{
			"player": steamIdentity(subject), "killer": steamIdentity(subject),
		}},
		map[string]any{"t": "conformance-profile.custom", "ts": at(6), "data": map[string]any{
			"target": map[string]any{"platform": "steam", "id": subject, "extra": 1},
		}},
	); err != nil {
		return err
	}

	page, err := env.playerEvents(ctx, env.AdminToken, subject, nil)
	if err != nil {
		return err
	}
	type expected struct {
		eventType, serverID string
		roles               []string
	}
	want := []expected{
		{"conformance-profile.custom", second.Server.Server.ID, []string{"target"}},
		{"core.player.death", second.Server.Server.ID, []string{"killer", "player"}},
		{"core.player.death", first.Server.Server.ID, []string{"killer"}},
		{"core.player.connect", first.Server.Server.ID, []string{"player"}},
	}
	if len(page.Events) != len(want) {
		types := make([]string, 0, len(page.Events))
		for _, event := range page.Events {
			types = append(types, event.Type)
		}
		return fmt.Errorf("the profile holds %v, want the four events whose data holds the identity at the top level, "+
			"newest first across both servers (sections 8.2 and 8.6)", types)
	}
	for i, event := range page.Events {
		if event.Type != want[i].eventType || event.ServerID != want[i].serverID {
			return fmt.Errorf("profile event %d is %s on %s, want %s on %s", i, event.Type, event.ServerID,
				want[i].eventType, want[i].serverID)
		}
		if !reflect.DeepEqual(event.Roles, want[i].roles) {
			return fmt.Errorf("profile event %d (%s) carries roles %v, want %v in name order", i, event.Type,
				event.Roles, want[i].roles)
		}
	}

	// A walk one page at a time meets each once.
	seen := map[string]bool{}
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > len(want) {
			return fmt.Errorf("a one-event walk did not end after %d pages", pages)
		}
		parameters := url.Values{"limit": {"1"}}
		if cursor != "" {
			parameters.Set("cursor", cursor)
		}
		one, err := env.playerEvents(ctx, env.AdminToken, subject, parameters)
		if err != nil {
			return err
		}
		for _, event := range one.Events {
			if seen[event.ID] {
				return fmt.Errorf("a one-event walk met %s twice", event.ID)
			}
			seen[event.ID] = true
		}
		if one.NextCursor == "" {
			break
		}
		cursor = one.NextCursor
	}
	if len(seen) != len(want) {
		return fmt.Errorf("a one-event walk met %d events, want %d", len(seen), len(want))
	}

	// Grants narrow it as they narrow the feed (section 10.3).
	deaths, err := env.mintToken(ctx, "conformance: profile deaths", "events:read:core.player.death")
	if err != nil {
		return err
	}
	narrowed, err := env.playerEvents(ctx, deaths.Secret, subject, nil)
	if err != nil {
		return err
	}
	if len(narrowed.Events) != 2 {
		return fmt.Errorf("a death-only token reads %d profile events, want the two deaths (section 10.3)", len(narrowed.Events))
	}
	if err := env.refused(ctx, http.MethodGet, playerPath("steam", subject, "/events?type=core.player.*"),
		deaths.Secret, nil, "an explicit type the grant does not cover"); err != nil {
		return err
	}

	// A binding leaves the other servers out, never refuses (section 10.2).
	bound, err := env.mintBoundToken(ctx, "conformance: profile bound", []string{first.Server.Server.ID}, "events:read")
	if err != nil {
		return err
	}
	confined, err := env.playerEvents(ctx, bound.Secret, subject, nil)
	if err != nil {
		return err
	}
	for _, event := range confined.Events {
		if event.ServerID != first.Server.Server.ID {
			return fmt.Errorf("a token bound to one server read a profile event of %s (section 10.2)", event.ServerID)
		}
	}
	if len(confined.Events) != 2 {
		return fmt.Errorf("a token bound to the first server reads %d profile events, want its two", len(confined.Events))
	}

	// An identity nobody has mentioned has an empty profile, and one over the
	// bounds is refused.
	if empty, err := env.playerEvents(ctx, env.AdminToken, uniqueIdentity(), nil); err != nil {
		return err
	} else if len(empty.Events) != 0 {
		return fmt.Errorf("an identity no event names has %d profile events, want none", len(empty.Events))
	}
	if err := env.expectError(ctx, http.MethodGet, playerPath(strings.Repeat("p", 65), subject, "/events"),
		env.AdminToken, nil, http.StatusBadRequest, "bad_request"); err != nil {
		return fmt.Errorf("a platform over 64 characters: %w", err)
	}
	return nil
}

func checkPlayerActions(ctx context.Context, env Env) error {
	first, err := env.newFakePlugin(ctx, "conformance: profile actions one", shortPollTimeoutSeconds)
	if err != nil {
		return err
	}
	second, err := env.newFakePlugin(ctx, "conformance: profile actions two", shortPollTimeoutSeconds)
	if err != nil {
		return err
	}
	for _, plugin := range []*fakePlugin{first, second} {
		if _, err := plugin.publishManifest(ctx, manifestBody(1)); err != nil {
			return err
		}
	}
	subject := uniqueIdentity()
	heal := func(serverID, referenceKey string) (string, error) {
		id, _, err := env.dispatchAction(ctx, serverID, map[string]any{
			"code": "example-mod.heal", "context": "player", "referenceKey": referenceKey,
			"params": map[string]any{"amount": 5},
		})
		return id, err
	}
	older, err := heal(first.Server.Server.ID, subject)
	if err != nil {
		return err
	}
	// Creation times are the order; a pause keeps the two apart.
	time.Sleep(20 * time.Millisecond)
	newer, err := heal(second.Server.Server.ID, subject)
	if err != nil {
		return err
	}
	if _, err := heal(first.Server.Server.ID, uniqueIdentity()); err != nil {
		return err
	}

	type actionsPage struct {
		Actions    []actionRecord `json:"actions"`
		NextCursor string         `json:"nextCursor"`
	}
	read := func(bearer, query string) (actionsPage, error) {
		var page actionsPage
		err := env.expect(ctx, http.MethodGet, playerPath("steam", subject, "/actions"+query), bearer, nil, http.StatusOK, &page)
		return page, err
	}
	page, err := read(env.AdminToken, "")
	if err != nil {
		return err
	}
	if len(page.Actions) != 2 || page.Actions[0].ID != newer || page.Actions[1].ID != older {
		return fmt.Errorf("the profile's actions are %+v, want the two dispatched against the identity, newest first (section 8.6)", page.Actions)
	}
	head, err := read(env.AdminToken, "?limit=1")
	if err != nil {
		return err
	}
	if len(head.Actions) != 1 || head.NextCursor == "" {
		return fmt.Errorf("a one-action page carries %d actions and cursor %q, want one and a cursor", len(head.Actions), head.NextCursor)
	}
	tail, err := read(env.AdminToken, "?limit=1&cursor="+url.QueryEscape(head.NextCursor))
	if err != nil {
		return err
	}
	if len(tail.Actions) != 1 || tail.Actions[0].ID != older || tail.NextCursor != "" {
		return fmt.Errorf("the second page is %+v, want the older action and no cursor", tail)
	}

	otherCodes, err := env.mintToken(ctx, "conformance: profile other codes", "actions:read:conformance-other.*")
	if err != nil {
		return err
	}
	if page, err := read(otherCodes.Secret, ""); err != nil {
		return err
	} else if len(page.Actions) != 0 {
		return fmt.Errorf("a token reading other codes sees %d of the identity's actions, want none: codes it cannot read are left out", len(page.Actions))
	}
	bound, err := env.mintBoundToken(ctx, "conformance: profile actions bound", []string{second.Server.Server.ID}, "actions:read")
	if err != nil {
		return err
	}
	if page, err := read(bound.Secret, ""); err != nil {
		return err
	} else if len(page.Actions) != 1 || page.Actions[0].ID != newer {
		return fmt.Errorf("a token bound to the second server sees %+v, want its one action", page.Actions)
	}
	return nil
}

type noteRecord struct {
	ID     string `json:"id"`
	Player struct {
		Platform string `json:"platform"`
		ID       string `json:"id"`
	} `json:"player"`
	Text      string `json:"text"`
	CreatedAt string `json:"createdAt"`
	CreatedBy struct {
		TokenID   string `json:"tokenId"`
		TokenName string `json:"tokenName"`
	} `json:"createdBy"`
}

func checkPlayerNotes(ctx context.Context, env Env) error {
	subject := uniqueIdentity()
	notesPath := playerPath("steam", subject, "/notes")
	writer, err := env.mintToken(ctx, "conformance-note-writer", "notes:write")
	if err != nil {
		return err
	}
	reader, err := env.mintToken(ctx, "conformance-note-reader", "notes:read")
	if err != nil {
		return err
	}

	var created struct {
		Note noteRecord `json:"note"`
	}
	if err := env.expect(ctx, http.MethodPost, notesPath, writer.Secret,
		map[string]any{"text": "Warned in direct chat."}, http.StatusCreated, &created); err != nil {
		return err
	}
	note := created.Note
	if note.ID == "" || note.Text != "Warned in direct chat." || note.Player.Platform != "steam" || note.Player.ID != subject {
		return fmt.Errorf("the created note is %+v, want the text on the identity", note)
	}
	if note.CreatedBy.TokenID != writer.Token.ID || note.CreatedBy.TokenName != "conformance-note-writer" {
		return fmt.Errorf("the note is credited to %+v, want the writing token by id and name (section 8.6)", note.CreatedBy)
	}
	if _, err := time.Parse(time.RFC3339, note.CreatedAt); err != nil {
		return fmt.Errorf("the note's createdAt %q is not RFC 3339", note.CreatedAt)
	}

	var listed struct {
		Notes []noteRecord `json:"notes"`
	}
	if err := env.expect(ctx, http.MethodGet, notesPath, reader.Secret, nil, http.StatusOK, &listed); err != nil {
		return err
	}
	if len(listed.Notes) != 1 || listed.Notes[0].ID != note.ID {
		return fmt.Errorf("the identity's notes are %+v, want the one written", listed.Notes)
	}

	// Each grant does its own half, and neither implies the other.
	if err := env.refused(ctx, http.MethodPost, notesPath, reader.Secret, map[string]any{"text": "x"},
		"writing a note with notes:read"); err != nil {
		return err
	}
	if err := env.refused(ctx, http.MethodGet, notesPath, writer.Secret, nil,
		"reading notes with notes:write alone"); err != nil {
		return err
	}
	for _, body := range []any{map[string]any{}, map[string]any{"text": " \n "}, map[string]any{"text": strings.Repeat("x", 4001)}} {
		if err := env.expectError(ctx, http.MethodPost, notesPath, writer.Secret, body,
			http.StatusBadRequest, "bad_request"); err != nil {
			return fmt.Errorf("note body %s: %w", truncate(mustMarshal(body)), err)
		}
	}

	// A note is deleted from its own identity only, and once.
	if err := env.expectError(ctx, http.MethodDelete, playerPath("steam", uniqueIdentity(), "/notes/"+note.ID),
		writer.Secret, nil, http.StatusNotFound, "not_found"); err != nil {
		return fmt.Errorf("deleting a note through another identity: %w", err)
	}
	if err := env.expect(ctx, http.MethodDelete, notesPath+"/"+note.ID, writer.Secret, nil, http.StatusNoContent, nil); err != nil {
		return err
	}
	if err := env.expectError(ctx, http.MethodDelete, notesPath+"/"+note.ID, writer.Secret, nil,
		http.StatusNotFound, "not_found"); err != nil {
		return fmt.Errorf("deleting a deleted note: %w", err)
	}
	return nil
}

func mustMarshal(value any) []byte {
	encoded, _ := json.Marshal(value)
	return encoded
}

func checkWebhookRedact(ctx context.Context, env Env) error {
	plugin, err := env.newFakePlugin(ctx, "conformance: webhook redaction", shortPollTimeoutSeconds)
	if err != nil {
		return err
	}
	receiver, err := env.startHookReceiver(http.StatusOK)
	if err != nil {
		return err
	}
	defer receiver.close()
	eventType := uniqueWebhookEventType()

	for _, bad := range [][]string{{""}, {"a..b"}, {"a.*"}, {"has space"}} {
		if err := env.expectError(ctx, http.MethodPost, "/api/v1/webhooks", env.AdminToken,
			map[string]any{"url": receiver.url, "redact": bad}, http.StatusBadRequest, "bad_request"); err != nil {
			return fmt.Errorf("redact %q: %w", bad, err)
		}
	}
	registered, err := env.registerWebhook(ctx, map[string]any{
		"url": receiver.url, "events": []string{eventType},
		"redact": []string{"position", "killer.position", "crew.position", "never.present"},
	})
	if err != nil {
		return err
	}
	defer func() { _ = env.deleteWebhook(context.WithoutCancel(ctx), registered.Webhook.ID) }()
	var view struct {
		Webhooks []struct {
			ID     string   `json:"id"`
			Redact []string `json:"redact"`
		} `json:"webhooks"`
	}
	if err := env.expect(ctx, http.MethodGet, "/api/v1/webhooks", env.AdminToken, nil, http.StatusOK, &view); err != nil {
		return err
	}
	for _, one := range view.Webhooks {
		if one.ID == registered.Webhook.ID && len(one.Redact) != 4 {
			return fmt.Errorf("the webhook's record carries redact %v, want the four paths registered", one.Redact)
		}
	}

	if _, err := plugin.sendEvents(ctx, map[string]any{"t": eventType, "data": map[string]any{
		"position": []float64{1, 2, 3},
		"killer":   map[string]any{"name": "K", "position": []float64{4, 5, 6}},
		"crew":     []any{map[string]any{"seat": 0, "position": []float64{7}}, "passenger", map[string]any{"seat": 1}},
		"weapon":   "M4A1",
	}}); err != nil {
		return err
	}
	hook, payload, err := receiver.awaitType(ctx, eventType, 30*time.Second)
	if err != nil {
		return err
	}
	if err := verifySignature(hook, registered.Secret); err != nil {
		return err
	}
	var data struct {
		Position *json.RawMessage `json:"position"`
		Killer   map[string]any   `json:"killer"`
		Crew     []any            `json:"crew"`
		Weapon   string           `json:"weapon"`
	}
	if err := json.Unmarshal(payload.Data, &data); err != nil {
		return fmt.Errorf("decode redacted data %s: %w", truncate(payload.Data), err)
	}
	if data.Position != nil || data.Killer["position"] != nil || bytes.Contains(payload.Data, []byte("[7]")) {
		return fmt.Errorf("the delivery's data %s still carries a redacted member (section 11.2)", truncate(payload.Data))
	}
	if data.Weapon != "M4A1" || data.Killer["name"] != "K" || len(data.Crew) != 3 || data.Crew[1] != "passenger" {
		return fmt.Errorf("the delivery's data %s lost members no path names", truncate(payload.Data))
	}
	stored, err := env.events(ctx, plugin.Server.Server.ID, url.Values{"type": {eventType}})
	if err != nil {
		return err
	}
	if len(stored.Events) != 1 || !bytes.Contains(compactJSON(stored.Events[0].Data), []byte("[1,2,3]")) {
		return fmt.Errorf("the stored event lost what redaction strips from deliveries; redaction filters what leaves, not what is kept")
	}
	return nil
}

func compactJSON(raw []byte) []byte {
	var buffer bytes.Buffer
	if json.Compact(&buffer, raw) != nil {
		return raw
	}
	return buffer.Bytes()
}

func checkWebhookAuditRecords(ctx context.Context, env Env) error {
	auditHook, err := env.startHookReceiver(http.StatusOK)
	if err != nil {
		return err
	}
	defer auditHook.close()
	everything, err := env.startHookReceiver(http.StatusOK)
	if err != nil {
		return err
	}
	defer everything.close()

	// Coverage first: only admin subscribes to the audit log, by name or by
	// namespace, while the same token can still subscribe to everything
	// else, which does not include it (section 11.2).
	manager, err := env.mintToken(ctx, "conformance: audit manager",
		"webhooks:manage", "events:read", "actions:read", "servers:read")
	if err != nil {
		return err
	}
	for _, filter := range [][]string{{"audit.*"}, {"audit.recorded"}} {
		if err := env.refused(ctx, http.MethodPost, "/api/v1/webhooks", manager.Secret,
			map[string]any{"url": auditHook.url, "events": filter},
			fmt.Sprintf("a non-admin subscribing to %v", filter)); err != nil {
			return err
		}
	}
	var catchAll registeredWebhook
	if err := env.expect(ctx, http.MethodPost, "/api/v1/webhooks", manager.Secret,
		map[string]any{"url": everything.url}, http.StatusCreated, &catchAll); err != nil {
		return fmt.Errorf("the catch-all is coverable without admin, since it excludes the audit notification: %w", err)
	}
	defer func() { _ = env.deleteWebhook(context.WithoutCancel(ctx), catchAll.Webhook.ID) }()

	registered, err := env.registerWebhook(ctx, map[string]any{"url": auditHook.url, "events": []string{"audit.*"}})
	if err != nil {
		return err
	}
	defer func() { _ = env.deleteWebhook(context.WithoutCancel(ctx), registered.Webhook.ID) }()

	// A mutation with something to find it by: a server registered under a
	// unique name. Its record names the server it created.
	name := "conformance audit " + uniqueIdentity()
	created, err := env.newServer(ctx, name)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(30 * time.Second)
	var found hookPayload
	var hook receivedHook
	for found.Type == "" {
		if time.Now().After(deadline) {
			return fmt.Errorf("no audit.recorded delivery for the server registration arrived within 30s (section 11.1)")
		}
		for i := 0; i < auditHook.count(); i++ {
			candidate := auditHook.get(i)
			var payload hookPayload
			if json.Unmarshal(candidate.Body, &payload) != nil || payload.Type != "audit.recorded" {
				continue
			}
			var record struct {
				Method   string `json:"method"`
				Path     string `json:"path"`
				ServerID string `json:"serverId"`
			}
			if json.Unmarshal(payload.Data, &record) == nil && record.Method == http.MethodPost &&
				record.Path == "/api/v1/servers" && record.ServerID == created.Server.ID {
				found, hook = payload, candidate
				break
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	if problem := hook.wellFormed(); problem != "" {
		return fmt.Errorf("the audit delivery was %s", problem)
	}
	if err := verifySignature(hook, registered.Secret); err != nil {
		return err
	}
	var record struct {
		ID        string `json:"id"`
		At        string `json:"at"`
		TokenName string `json:"tokenName"`
		Status    int    `json:"status"`
		SourceIP  string `json:"sourceIp"`
	}
	if err := json.Unmarshal(found.Data, &record); err != nil {
		return fmt.Errorf("decode audit delivery data %s: %w", truncate(found.Data), err)
	}
	if record.ID == "" || record.Status != http.StatusCreated || record.SourceIP == "" || record.At == "" {
		return fmt.Errorf("the audit delivery's data %s is not the record as GET /api/v1/audit answers it", truncate(found.Data))
	}
	page, err := env.auditRecords(ctx, url.Values{"serverId": {created.Server.ID}})
	if err != nil {
		return err
	}
	matched := false
	for _, one := range page.Records {
		if one.ID == record.ID {
			matched = true
		}
	}
	if !matched {
		return fmt.Errorf("the delivered audit record %s is not in the audit log", record.ID)
	}

	// The catch-all heard none of it. It was registered before the server,
	// so anything it could match has had the same time to arrive.
	time.Sleep(3 * time.Second)
	if count := everything.countOfType("audit.recorded"); count != 0 {
		return fmt.Errorf("a webhook with no filter received %d audit.recorded deliveries; the catch-all never matches it (section 11.1)", count)
	}

	// A non-admin may not send an audit body again either (section 11.5).
	delivery, err := env.awaitDelivery(ctx, registered.Webhook.ID, 10*time.Second, "an audit delivery",
		func(one deliveryRecord) bool { return one.Type == "audit.recorded" })
	if err != nil {
		return err
	}
	return env.refused(ctx, http.MethodPost,
		"/api/v1/webhooks/"+registered.Webhook.ID+"/deliveries/"+delivery.ID+"/replay", manager.Secret, nil,
		"a non-admin replaying an audit delivery")
}

var playerChecks = []Check{
	{
		ID:      "plugin.events.reservedNamespaces",
		Title:   "Telemetry in the action, server, and audit namespaces is refused",
		Section: "8.1",
		Run:     checkReservedNamespaces,
	},
	{
		ID:      "admin.players.events",
		Title:   "A player profile finds every top-level reference across servers, narrowed like the feed",
		Section: "8.2, 8.6",
		Run:     checkPlayerEvents,
	},
	{
		ID:      "admin.players.actions",
		Title:   "A player profile lists the player-context actions against the id, narrowed to readable codes",
		Section: "8.6",
		Run:     checkPlayerActions,
	},
	{
		ID:      "admin.players.notes",
		Title:   "Notes are credited to their writer, gated by their own grants, and deleted from their identity only",
		Section: "8.6",
		Run:     checkPlayerNotes,
	},
	{
		ID:      "webhooks.redact",
		Title:   "Redaction paths strip members from deliveries and leave the stored event whole",
		Section: "11.2",
		Run:     checkWebhookRedact,
	},
	{
		ID:      "webhooks.auditRecords",
		Title:   "Audit records reach a webhook naming them, never the catch-all, and only admin subscribes",
		Section: "11.1, 11.2",
		Run:     checkWebhookAuditRecords,
	},
}

func init() {
	checks = append(checks, playerChecks...)
}
