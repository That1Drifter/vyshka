package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Checks for the installation ban list (spec section 13, draft 0.32): the Admin
// API that manages it, the paged read a plugin walks one revision at a time,
// the bans.changed nudge that goes only to a plugin declaring the capability,
// the bans.applied report the server record keeps, and expiry.
//
// A hub under test may be long-lived and already hold bans, so nothing here
// assumes an absolute revision or an empty list: every identity is minted
// fresh (uniqueIdentity) and every revision is compared with one read earlier.

type banAuthorRecord struct {
	TokenID   string `json:"tokenId"`
	TokenName string `json:"tokenName"`
}

type banRecord struct {
	ID     string `json:"id"`
	Player struct {
		Platform string `json:"platform"`
		ID       string `json:"id"`
	} `json:"player"`
	Reason    string           `json:"reason"`
	Name      string           `json:"name"`
	ServerID  *string          `json:"serverId"`
	CreatedAt string           `json:"createdAt"`
	CreatedBy banAuthorRecord  `json:"createdBy"`
	ExpiresAt *string          `json:"expiresAt"`
	State     string           `json:"state"`
	LiftedAt  *string          `json:"liftedAt"`
	LiftedBy  *banAuthorRecord `json:"liftedBy"`
}

type banChangeRecord struct {
	Ban      banRecord `json:"ban"`
	Revision int64     `json:"revision"`
}

type banPage struct {
	Revision   int64       `json:"revision"`
	Bans       []banRecord `json:"bans"`
	NextCursor string      `json:"nextCursor"`
}

type pluginBanEntry struct {
	ID     string `json:"id"`
	Player struct {
		Platform string `json:"platform"`
		ID       string `json:"id"`
	} `json:"player"`
	Reason    string  `json:"reason"`
	Name      string  `json:"name"`
	ExpiresAt *string `json:"expiresAt"`
}

type pluginBanPage struct {
	Revision   int64            `json:"revision"`
	Bans       []pluginBanEntry `json:"bans"`
	NextCursor string           `json:"nextCursor"`
}

func banRequest(playerID, reason string, extra map[string]any) map[string]any {
	body := map[string]any{"player": steamIdentity(playerID), "reason": reason}
	for key, value := range extra {
		body[key] = value
	}
	return body
}

func (e Env) createBan(ctx context.Context, bearer string, body map[string]any) (banChangeRecord, error) {
	var change banChangeRecord
	err := e.expect(ctx, http.MethodPost, "/api/v1/bans", bearer, body, http.StatusCreated, &change)
	if err == nil && (change.Ban.ID == "" || change.Ban.State != "active") {
		err = fmt.Errorf("POST /api/v1/bans answered %+v, want an active ban with an id", change.Ban)
	}
	return change, err
}

func (e Env) liftBan(ctx context.Context, bearer, banID string) (banChangeRecord, error) {
	var change banChangeRecord
	err := e.expect(ctx, http.MethodPost, "/api/v1/bans/"+url.PathEscape(banID)+"/lift", bearer, nil, http.StatusOK, &change)
	return change, err
}

func (e Env) listBans(ctx context.Context, bearer string, parameters url.Values) (banPage, error) {
	path := "/api/v1/bans"
	if encoded := parameters.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var page banPage
	err := e.expect(ctx, http.MethodGet, path, bearer, nil, http.StatusOK, &page)
	return page, err
}

// pullBans reads one page of the plugin's walk, on the GET spelling or the
// POST one (spec section 13.3).
func (e Env) pullBans(ctx context.Context, sessionToken string, post bool, parameters url.Values) (pluginBanPage, error) {
	method, path := http.MethodGet, "/plugin/v1/bans"
	if post {
		method, path = http.MethodPost, "/plugin/v1/bans/get"
	}
	if encoded := parameters.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var page pluginBanPage
	resp, body, err := e.do(ctx, method, path, sessionToken, nil)
	if err != nil {
		return page, fmt.Errorf("%s %s: %w", method, path, err)
	}
	if resp.StatusCode != http.StatusOK {
		return page, fmt.Errorf("%s %s: want status 200, got %d, body %q", method, path, resp.StatusCode, truncate(body))
	}
	if err := json.Unmarshal(body, &page); err != nil {
		return page, fmt.Errorf("%s %s: decode body %q: %w", method, path, truncate(body), err)
	}
	if page.Bans == nil {
		return page, fmt.Errorf("%s %s: bans is absent or null; an empty page is []", method, path)
	}
	for _, c := range page.NextCursor {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return page, fmt.Errorf("%s %s: nextCursor %q carries %q; a cursor is drawn from letters, digits, - and _ alone (section 13.3)",
				method, path, page.NextCursor, c)
		}
	}
	return page, nil
}

// banKey is one entry's identity as a walk meets it, platform and id, which
// is what a walk is ordered by (section 13.3).
type banKey struct{ platform, id string }

func (k banKey) String() string { return k.platform + ":" + k.id }

// before reports whether k comes before other in the byte order of platform
// then id.
func (k banKey) before(other banKey) bool {
	if k.platform != other.platform {
		return k.platform < other.platform
	}
	return k.id < other.id
}

// containsSteam reports whether a walk met the steam identity id.
func containsSteam(met []banKey, id string) bool {
	for _, key := range met {
		if key.platform == "steam" && key.id == id {
			return true
		}
	}
	return false
}

// walkBans reads the list from the first page (or from cursor) to the end,
// holding the walk to one revision: every page must say the revision the
// first one did. It returns that revision and the identities met, in order.
func (e Env) walkBans(ctx context.Context, sessionToken string, post bool, cursor string, revision int64, limit int) (int64, []banKey, error) {
	var met []banKey
	for pages := 0; ; pages++ {
		if pages > 10000 {
			return 0, nil, fmt.Errorf("a walk of the ban list did not end after %d pages", pages)
		}
		parameters := url.Values{"limit": {fmt.Sprint(limit)}}
		if cursor != "" {
			parameters.Set("cursor", cursor)
		}
		page, err := e.pullBans(ctx, sessionToken, post, parameters)
		if err != nil {
			return 0, nil, err
		}
		if revision < 0 {
			revision = page.Revision
		} else if page.Revision != revision {
			return 0, nil, fmt.Errorf("a page of a walk that began at revision %d was served at %d; every page reached through a walk's cursors is served at the revision it began at (section 13.3)",
				revision, page.Revision)
		}
		for _, entry := range page.Bans {
			met = append(met, banKey{entry.Player.Platform, entry.Player.ID})
		}
		if page.NextCursor == "" {
			return revision, met, nil
		}
		cursor = page.NextCursor
	}
}

func checkBanLifecycle(ctx context.Context, env Env) error {
	subject := uniqueIdentity()
	before, err := env.listBans(ctx, env.AdminToken, url.Values{"limit": {"1"}})
	if err != nil {
		return err
	}
	placed, err := env.createBan(ctx, env.AdminToken, banRequest(subject, "conformance: speed hack", map[string]any{"name": "Anna"}))
	if err != nil {
		return err
	}
	if placed.Revision <= before.Revision {
		return fmt.Errorf("placing a ban answered revision %d, not above the %d the list stood at; every change moves the revision (section 13.1)",
			placed.Revision, before.Revision)
	}
	ban := placed.Ban
	if ban.Player.Platform != "steam" || ban.Player.ID != subject || ban.Reason != "conformance: speed hack" || ban.Name != "Anna" ||
		ban.ExpiresAt != nil || ban.ServerID != nil || ban.LiftedAt != nil || ban.LiftedBy != nil || ban.CreatedAt == "" {
		return fmt.Errorf("the placed ban reads %+v, want the identity, reason and name as sent, no expiry, no provenance, never lifted", ban)
	}

	var conflict struct {
		Error struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	resp, body, err := env.do(ctx, http.MethodPost, "/api/v1/bans", env.AdminToken, banRequest(subject, "again", nil))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusConflict || json.Unmarshal(body, &conflict) != nil ||
		conflict.Error.Code != "conflict" || conflict.Error.Details["banId"] != ban.ID {
		return fmt.Errorf("a second ban of an identity under an active ban answered %d %s, want 409 conflict with details.banId %s (section 13.2)",
			resp.StatusCode, truncate(body), ban.ID)
	}

	history, err := env.listBans(ctx, env.AdminToken, url.Values{"platform": {"steam"}, "playerId": {subject}})
	if err != nil {
		return err
	}
	if len(history.Bans) != 1 || history.Bans[0].ID != ban.ID || history.Revision < placed.Revision {
		return fmt.Errorf("the active bans of the identity are %+v at revision %d, want the one ban at %d or later",
			history.Bans, history.Revision, placed.Revision)
	}

	lifted, err := env.liftBan(ctx, env.AdminToken, ban.ID)
	if err != nil {
		return err
	}
	if lifted.Ban.State != "lifted" || lifted.Ban.LiftedAt == nil || lifted.Ban.LiftedBy == nil || lifted.Revision <= placed.Revision {
		return fmt.Errorf("the lift answered %+v at revision %d, want a lifted ban with liftedAt and liftedBy above revision %d",
			lifted.Ban, lifted.Revision, placed.Revision)
	}
	again, err := env.liftBan(ctx, env.AdminToken, ban.ID)
	if err != nil {
		return fmt.Errorf("a second lift of the same ban: %w; a lift of a ban no longer active answers the record as it stands", err)
	}
	if again.Ban.State != "lifted" || again.Ban.LiftedAt == nil || *again.Ban.LiftedAt != *lifted.Ban.LiftedAt {
		return fmt.Errorf("a second lift answered %+v; it changes nothing, the recorded lift time included (section 13.2)", again.Ban)
	}

	active, err := env.listBans(ctx, env.AdminToken, url.Values{"platform": {"steam"}, "playerId": {subject}})
	if err != nil {
		return err
	}
	if len(active.Bans) != 0 {
		return fmt.Errorf("after the lift the identity's active bans are %+v, want none", active.Bans)
	}
	all, err := env.listBans(ctx, env.AdminToken, url.Values{"platform": {"steam"}, "playerId": {subject}, "state": {"all"}})
	if err != nil {
		return err
	}
	if len(all.Bans) != 1 || all.Bans[0].State != "lifted" {
		return fmt.Errorf("state=all lists %+v for the identity, want the lifted ban: records are kept after a lift (section 13.1)", all.Bans)
	}
	var one struct {
		Ban banRecord `json:"ban"`
	}
	if err := env.expect(ctx, http.MethodGet, "/api/v1/bans/"+url.PathEscape(ban.ID), env.AdminToken, nil, http.StatusOK, &one); err != nil {
		return err
	}
	if one.Ban.ID != ban.ID || one.Ban.State != "lifted" {
		return fmt.Errorf("GET /api/v1/bans/%s reads %+v", ban.ID, one.Ban)
	}
	if _, err := env.createBan(ctx, env.AdminToken, banRequest(subject, "conformance: again after the lift", nil)); err != nil {
		return fmt.Errorf("banning the identity again after the lift: %w", err)
	}

	// Provenance and a duration: expiresAt counted from the hub's clock, and
	// the server named.
	created, err := env.newServer(ctx, "conformance: ban provenance")
	if err != nil {
		return err
	}
	timed, err := env.createBan(ctx, env.AdminToken, banRequest(uniqueIdentity(), "conformance: timed", map[string]any{
		"durationSeconds": 3600, "serverId": created.Server.ID,
	}))
	if err != nil {
		return err
	}
	if timed.Ban.ServerID == nil || *timed.Ban.ServerID != created.Server.ID {
		return fmt.Errorf("a ban naming server %s reads serverId %v", created.Server.ID, timed.Ban.ServerID)
	}
	if timed.Ban.ExpiresAt == nil {
		return fmt.Errorf("a ban with durationSeconds carries no expiresAt")
	}
	expires, err := time.Parse(time.RFC3339, *timed.Ban.ExpiresAt)
	placedAt, placedErr := time.Parse(time.RFC3339, timed.Ban.CreatedAt)
	if err != nil || placedErr != nil || expires.Sub(placedAt) < time.Hour-time.Second || expires.Sub(placedAt) > time.Hour+time.Second {
		return fmt.Errorf("a one-hour ban created at %s expires at %s, want an hour later", timed.Ban.CreatedAt, *timed.Ban.ExpiresAt)
	}
	if _, err := env.liftBan(ctx, env.AdminToken, timed.Ban.ID); err != nil {
		return err
	}

	// Refusals change nothing.
	for _, bad := range []struct {
		why  string
		body map[string]any
		code string
		want int
	}{
		{"no player", map[string]any{"reason": "x"}, "bad_request", http.StatusBadRequest},
		{"no reason", map[string]any{"player": steamIdentity(uniqueIdentity())}, "bad_request", http.StatusBadRequest},
		{"a blank reason", banRequest(uniqueIdentity(), " ", nil), "bad_request", http.StatusBadRequest},
		{"a reason over 200 characters", banRequest(uniqueIdentity(), strings.Repeat("r", 201), nil), "bad_request", http.StatusBadRequest},
		{"an id over 128 characters", banRequest(strings.Repeat("i", 129), "x", nil), "bad_request", http.StatusBadRequest},
		{"a zero duration", banRequest(uniqueIdentity(), "x", map[string]any{"durationSeconds": 0}), "bad_request", http.StatusBadRequest},
		{"a duration past ten years", banRequest(uniqueIdentity(), "x", map[string]any{"durationSeconds": 315360001}), "bad_request", http.StatusBadRequest},
		{"an unknown serverId", banRequest(uniqueIdentity(), "x", map[string]any{"serverId": "conformance-no-such-server"}), "not_found", http.StatusNotFound},
	} {
		if err := env.expectError(ctx, http.MethodPost, "/api/v1/bans", env.AdminToken, bad.body, bad.want, bad.code); err != nil {
			return fmt.Errorf("a ban with %s: %w", bad.why, err)
		}
	}
	for _, query := range []string{"state=lifted", "platform=steam", "playerId=x"} {
		if err := env.expectError(ctx, http.MethodGet, "/api/v1/bans?"+query, env.AdminToken, nil,
			http.StatusBadRequest, "bad_request"); err != nil {
			return fmt.Errorf("GET /api/v1/bans?%s: %w", query, err)
		}
	}
	return env.expectError(ctx, http.MethodPost, "/api/v1/bans/conformance-no-such-ban/lift", env.AdminToken, nil,
		http.StatusNotFound, "not_found")
}

func checkBanScopes(ctx context.Context, env Env) error {
	reader, err := env.mintToken(ctx, "conformance: ban reader", "bans:read")
	if err != nil {
		return err
	}
	manager, err := env.mintToken(ctx, "conformance: ban manager", "bans:manage")
	if err != nil {
		return err
	}
	if err := env.refused(ctx, http.MethodPost, "/api/v1/bans", reader.Secret, banRequest(uniqueIdentity(), "x", nil),
		"bans:read placing a ban"); err != nil {
		return err
	}
	placed, err := env.createBan(ctx, manager.Secret, banRequest(uniqueIdentity(), "conformance: by the manager", nil))
	if err != nil {
		return err
	}
	if placed.Ban.CreatedBy.TokenID != manager.Token.ID || placed.Ban.CreatedBy.TokenName != "conformance: ban manager" {
		return fmt.Errorf("createdBy = %+v, want the manager token", placed.Ban.CreatedBy)
	}
	if _, err := env.listBans(ctx, manager.Secret, url.Values{"limit": {"1"}}); err != nil {
		return fmt.Errorf("bans:manage reading the list: %w; managing implies reading (section 10.1)", err)
	}
	if _, err := env.listBans(ctx, reader.Secret, url.Values{"limit": {"1"}}); err != nil {
		return fmt.Errorf("bans:read reading the list: %w", err)
	}
	if err := env.refused(ctx, http.MethodPost, "/api/v1/bans/"+url.PathEscape(placed.Ban.ID)+"/lift", reader.Secret, nil,
		"bans:read lifting a ban"); err != nil {
		return err
	}
	if _, err := env.liftBan(ctx, manager.Secret, placed.Ban.ID); err != nil {
		return err
	}

	created, err := env.newServer(ctx, "conformance: ban binding")
	if err != nil {
		return err
	}
	if err := env.expectError(ctx, http.MethodPost, "/api/v1/tokens", env.AdminToken, map[string]any{
		"name": "conformance: bound banner", "scopes": []string{"bans:manage"}, "servers": []string{created.Server.ID},
	}, http.StatusBadRequest, "bad_request"); err != nil {
		return fmt.Errorf("minting bans:manage onto a bound token: %w; the grant reaches every server (section 10.1)", err)
	}
	for _, scope := range []string{"bans:read:steam", "bans:manage:*"} {
		if err := env.expectError(ctx, http.MethodPost, "/api/v1/tokens", env.AdminToken, map[string]any{
			"name": "conformance: patterned ban grant", "scopes": []string{scope},
		}, http.StatusBadRequest, "bad_request"); err != nil {
			return fmt.Errorf("minting %s: %w; neither bans grant takes a pattern (section 10.1)", scope, err)
		}
	}
	bound, err := env.mintBoundToken(ctx, "conformance: bound ban reader", []string{created.Server.ID}, "bans:read")
	if err != nil {
		return fmt.Errorf("minting bans:read onto a bound token: %w; the binding does not narrow it (section 10.1)", err)
	}
	if _, err := env.listBans(ctx, bound.Secret, url.Values{"limit": {"1"}}); err != nil {
		return fmt.Errorf("a bound bans:read token reading the list: %w", err)
	}
	notes, err := env.mintToken(ctx, "conformance: no ban grant", "notes:read")
	if err != nil {
		return err
	}
	return env.refused(ctx, http.MethodGet, "/api/v1/bans", notes.Secret, nil, "a token without a bans grant reading the list")
}

func checkBanPull(ctx context.Context, env Env) error {
	subjects := []string{uniqueIdentity(), uniqueIdentity(), uniqueIdentity()}
	for _, subject := range subjects {
		if _, err := env.createBan(ctx, env.AdminToken, banRequest(subject, "conformance: pulled "+subject, map[string]any{"name": "n"})); err != nil {
			return err
		}
	}
	plugin, err := env.newFakePlugin(ctx, "conformance: ban puller", shortPollTimeoutSeconds)
	if err != nil {
		return err
	}
	current, err := env.listBans(ctx, env.AdminToken, url.Values{"limit": {"1"}})
	if err != nil {
		return err
	}
	if plugin.Session.Server.BansRevision == nil {
		return fmt.Errorf("the session response carries no server.bansRevision; a hub implementing section 13 reports it on every session response (section 5.3)")
	}
	if *plugin.Session.Server.BansRevision > current.Revision {
		return fmt.Errorf("the session response reported bansRevision %d, above the %d the list read afterwards", *plugin.Session.Server.BansRevision, current.Revision)
	}

	for _, post := range []bool{false, true} {
		spelling := "GET /plugin/v1/bans"
		if post {
			spelling = "POST /plugin/v1/bans/get"
		}
		first, err := env.pullBans(ctx, plugin.Session.SessionToken, post, url.Values{"limit": {"1"}})
		if err != nil {
			return err
		}
		// The list changes under the walk: one of the subjects is lifted and
		// a new identity banned. The walk still reads the revision it began at.
		var liftID string
		history, err := env.listBans(ctx, env.AdminToken, url.Values{"platform": {"steam"}, "playerId": {subjects[1]}})
		if err != nil {
			return err
		}
		if len(history.Bans) != 1 {
			return fmt.Errorf("%s: the ban of %s is not active", spelling, subjects[1])
		}
		liftID = history.Bans[0].ID
		if _, err := env.liftBan(ctx, env.AdminToken, liftID); err != nil {
			return err
		}
		late := uniqueIdentity()
		if _, err := env.createBan(ctx, env.AdminToken, banRequest(late, "conformance: late", nil)); err != nil {
			return err
		}

		var met []banKey
		for _, entry := range first.Bans {
			met = append(met, banKey{entry.Player.Platform, entry.Player.ID})
		}
		if first.NextCursor != "" {
			_, rest, err := env.walkBans(ctx, plugin.Session.SessionToken, post, first.NextCursor, first.Revision, 500)
			if err != nil {
				return fmt.Errorf("%s: %w", spelling, err)
			}
			met = append(met, rest...)
		}
		for _, subject := range subjects {
			if !containsSteam(met, subject) {
				return fmt.Errorf("%s: a walk that began at revision %d did not meet %s, which was on the list then; a walk reads its revision whole (section 13.3)",
					spelling, first.Revision, subject)
			}
		}
		if containsSteam(met, late) {
			return fmt.Errorf("%s: a walk that began at revision %d met %s, banned after it began", spelling, first.Revision, late)
		}
		for i := 1; i < len(met); i++ {
			if !met[i-1].before(met[i]) {
				return fmt.Errorf("%s: the walk met %q after %q; entries come in byte order of platform then id (section 13.3)",
					spelling, met[i], met[i-1])
			}
		}

		revision, fresh, err := env.walkBans(ctx, plugin.Session.SessionToken, post, "", -1, 500)
		if err != nil {
			return fmt.Errorf("%s: %w", spelling, err)
		}
		if revision <= first.Revision || containsSteam(fresh, subjects[1]) || !containsSteam(fresh, late) {
			return fmt.Errorf("%s: a fresh walk read revision %d (the earlier walk %d), met the lifted %s: %v, met the late %s: %v",
				spelling, revision, first.Revision, subjects[1], containsSteam(fresh, subjects[1]), late, containsSteam(fresh, late))
		}
		// Put the lifted subject back for the other spelling's walk.
		if _, err := env.createBan(ctx, env.AdminToken, banRequest(subjects[1], "conformance: back", nil)); err != nil {
			return err
		}
	}

	if err := env.expectError(ctx, http.MethodGet, "/plugin/v1/bans?cursor=not-a-cursor-this-hub-issued", plugin.Session.SessionToken, nil,
		http.StatusBadRequest, "bad_request"); err != nil {
		return fmt.Errorf("a cursor the hub never issued: %w", err)
	}
	if _, err := env.expectInline(ctx, env.Client, http.MethodPost, "/plugin/v1/bans/get?cursor=not-a-cursor-this-hub-issued&errors=inline",
		plugin.Session.SessionToken, nil, "bad_request", http.StatusBadRequest); err != nil {
		return fmt.Errorf("the POST spelling asked for inline errors: %w", err)
	}
	return env.expectError(ctx, http.MethodGet, "/plugin/v1/bans", "", nil, http.StatusUnauthorized, "session_invalid")
}

// capableManifest is the suite's manifest declaring the bans capability, an
// entry this suite made up beside it, which a hub must ignore (section 6.7).
func capableManifest(revision int64, capable bool) map[string]any {
	manifest := manifestBody(revision)
	if capable {
		manifest["capabilities"] = []string{"bans", "conformance-unknown-capability"}
	}
	return manifest
}

type serverBansRecord struct {
	Bans *struct {
		Supported       bool    `json:"supported"`
		AppliedRevision *int64  `json:"appliedRevision"`
		AppliedAt       *string `json:"appliedAt"`
	} `json:"bans"`
}

func (e Env) serverBans(ctx context.Context, serverID string) (serverBansRecord, error) {
	var record serverBansRecord
	if err := e.expect(ctx, http.MethodGet, "/api/v1/servers/"+serverID, e.AdminToken, nil, http.StatusOK, &record); err != nil {
		return record, err
	}
	if record.Bans == nil {
		return record, fmt.Errorf("the server record carries no bans member (section 5.1)")
	}
	return record, nil
}

func checkBanChanged(ctx context.Context, env Env) error {
	capable, err := env.newFakePlugin(ctx, "conformance: ban-capable", shortPollTimeoutSeconds)
	if err != nil {
		return err
	}
	plain, err := env.newFakePlugin(ctx, "conformance: not ban-capable", shortPollTimeoutSeconds)
	if err != nil {
		return err
	}
	if _, err := capable.publishManifest(ctx, capableManifest(1, true)); err != nil {
		return err
	}
	if _, err := plain.publishManifest(ctx, capableManifest(1, false)); err != nil {
		return err
	}
	if record, err := env.serverBans(ctx, capable.Server.Server.ID); err != nil {
		return err
	} else if !record.Bans.Supported {
		return fmt.Errorf("a server whose manifest declares bans (beside an unknown entry) shows bans.supported false (sections 5.1 and 6.7)")
	}
	if record, err := env.serverBans(ctx, plain.Server.Server.ID); err != nil {
		return err
	} else if record.Bans.Supported {
		return fmt.Errorf("a server whose manifest does not declare bans shows bans.supported true")
	}

	placed, err := env.createBan(ctx, env.AdminToken, banRequest(uniqueIdentity(), "conformance: nudge", nil))
	if err != nil {
		return err
	}
	notices, err := capable.awaitEnvelope(ctx, "bans.changed", 6)
	if err != nil {
		return fmt.Errorf("%w; a change to the list queues bans.changed for every server whose manifest declares the capability (section 13.3)", err)
	}
	var body struct {
		Revision *int64 `json:"revision"`
	}
	last := notices[len(notices)-1]
	if json.Unmarshal(last.Body, &body) != nil || body.Revision == nil || *body.Revision < placed.Revision {
		return fmt.Errorf("the bans.changed arrived as %s, want a revision of at least %d", last.Body, placed.Revision)
	}

	// The server without the capability: one poll, held to its timeout if
	// nothing is queued, must bring no notice.
	response, err := plain.pollAndAck(ctx)
	if err != nil {
		return err
	}
	for _, delivered := range response.Envelopes {
		if delivered.Type == "bans.changed" {
			return fmt.Errorf("a server whose manifest does not declare bans was sent a bans.changed; an old plugin acks what it does not understand, so the hub sends it nothing (section 13.3)")
		}
	}
	if err := env.expectError(ctx, http.MethodPost, "/api/v1/servers/"+plain.Server.Server.ID+"/envelopes", env.AdminToken,
		map[string]any{"type": "bans.changed", "body": map[string]any{"revision": 1}}, http.StatusConflict, "conflict"); err != nil {
		return fmt.Errorf("queueing a raw bans.changed: %w; the family belongs to the hub (section 5.5)", err)
	}
	_, err = env.liftBan(ctx, env.AdminToken, placed.Ban.ID)
	return err
}

func checkBansApplied(ctx context.Context, env Env) error {
	plugin, err := env.newFakePlugin(ctx, "conformance: ban reporter", shortPollTimeoutSeconds)
	if err != nil {
		return err
	}
	if _, err := plugin.publishManifest(ctx, capableManifest(1, true)); err != nil {
		return err
	}
	record, err := env.serverBans(ctx, plugin.Server.Server.ID)
	if err != nil {
		return err
	}
	if record.Bans.AppliedRevision != nil || record.Bans.AppliedAt != nil {
		return fmt.Errorf("before any report the server shows applied revision %v at %v, want both null (section 5.1)",
			record.Bans.AppliedRevision, record.Bans.AppliedAt)
	}
	reports := []envelope{
		plugin.nextOutbound("bans.applied", map[string]any{"revision": 4}),
		plugin.nextOutbound("bans.applied", map[string]any{"revision": 7}),
		plugin.nextOutbound("bans.applied", map[string]any{"revision": 1.5}),
		plugin.nextOutbound("bans.applied", map[string]any{"revision": -3}),
		plugin.nextOutbound("bans.applied", map[string]any{}),
	}
	response, err := plugin.send(ctx, reports...)
	if err != nil {
		return fmt.Errorf("a poll carrying bans.applied reports failed: %w; an unusable report is acked and ignored (section 13.4)", err)
	}
	if response.Ack < reports[len(reports)-1].Seq {
		return fmt.Errorf("ack = %d after the reports, want %d: an unusable report is acked like any envelope", response.Ack, reports[len(reports)-1].Seq)
	}
	record, err = env.serverBans(ctx, plugin.Server.Server.ID)
	if err != nil {
		return err
	}
	if record.Bans.AppliedRevision == nil || *record.Bans.AppliedRevision != 7 || record.Bans.AppliedAt == nil {
		return fmt.Errorf("after reports of 4 then 7 (and three unusable ones) the server shows applied revision %v at %v, want 7: the last usable report wins (section 13.4)",
			record.Bans.AppliedRevision, record.Bans.AppliedAt)
	}
	return nil
}

func checkBanExpiry(ctx context.Context, env Env) error {
	plugin, err := env.newFakePlugin(ctx, "conformance: ban expiry", shortPollTimeoutSeconds)
	if err != nil {
		return err
	}
	subject := uniqueIdentity()
	placed, err := env.createBan(ctx, env.AdminToken, banRequest(subject, "conformance: brief", map[string]any{"durationSeconds": 2}))
	if err != nil {
		return err
	}
	// Expired on every Admin API read from its expiry on, sweep or no sweep.
	time.Sleep(2500 * time.Millisecond)
	var one struct {
		Ban banRecord `json:"ban"`
	}
	if err := env.expect(ctx, http.MethodGet, "/api/v1/bans/"+url.PathEscape(placed.Ban.ID), env.AdminToken, nil, http.StatusOK, &one); err != nil {
		return err
	}
	if one.Ban.State != "expired" || one.Ban.LiftedAt != nil {
		return fmt.Errorf("a ban past its expiry reads %+v, want expired and never lifted (section 13.1)", one.Ban)
	}
	// Off the served list within the 60 s the hub is allowed, with the
	// revision moving.
	deadline := time.Now().Add(65 * time.Second)
	for {
		revision, met, err := env.walkBans(ctx, plugin.Session.SessionToken, false, "", -1, 500)
		if err != nil {
			return err
		}
		if !containsSteam(met, subject) {
			if revision <= placed.Revision {
				return fmt.Errorf("the expired ban left the list without the revision moving (%d, placed at %d)", revision, placed.Revision)
			}
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the ban was still on the served list 65 s after its expiry; a hub takes an expired ban off within 60 s (section 13.1)")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func checkManifestCapabilityBounds(ctx context.Context, env Env) error {
	plugin, err := env.newFakePlugin(ctx, "conformance: capability bounds", shortPollTimeoutSeconds)
	if err != nil {
		return err
	}
	many := make([]string, 33)
	for i := range many {
		many[i] = fmt.Sprintf("conformance-capability-%d", i)
	}
	for revision, bad := range []any{many, []string{""}, []string{strings.Repeat("c", 65)}, "bans"} {
		manifest := manifestBody(int64(revision + 1))
		manifest["capabilities"] = bad
		response, err := plugin.publishManifest(ctx, manifest)
		if err != nil {
			return err
		}
		rejected := false
		for _, delivered := range response.Envelopes {
			if delivered.Type == "manifest.reject" {
				rejected = true
			}
		}
		if !rejected {
			if more, err := plugin.awaitEnvelope(ctx, "manifest.reject", 2); err != nil || len(more) == 0 {
				return fmt.Errorf("a manifest with capabilities %v was not rejected; a list outside the section 6.7 bounds rejects the manifest", bad)
			}
		}
	}
	return nil
}

var banChecks = []Check{
	{
		ID:      "admin.bans.lifecycle",
		Title:   "A ban is placed, refused twice, lifted once, kept, and placed again, the revision moving each time",
		Section: "13.1, 13.2",
		Run:     checkBanLifecycle,
	},
	{
		ID:      "admin.bans.scopes",
		Title:   "Reading and managing the list are separate grants, and managing cannot be bound",
		Section: "10.1, 13.2",
		Run:     checkBanScopes,
	},
	{
		ID:      "plugin.bans.pull",
		Title:   "A walk of the ban list reads one revision whole, on both spellings",
		Section: "5.3, 13.3",
		Run:     checkBanPull,
	},
	{
		ID:      "plugin.bans.changed",
		Title:   "bans.changed reaches the servers that declare the capability and no other",
		Section: "6.7, 13.3",
		Run:     checkBanChanged,
	},
	{
		ID:      "plugin.bans.applied",
		Title:   "The last usable bans.applied report lands on the server record",
		Section: "5.1, 13.4",
		Run:     checkBansApplied,
	},
	{
		ID:      "plugin.bans.expiry",
		Title:   "An expired ban reads expired at once and leaves the served list within 60 s",
		Section: "13.1",
		Run:     checkBanExpiry,
	},
	{
		ID:      "plugin.manifest.capabilities",
		Title:   "A capability list outside its bounds rejects the manifest",
		Section: "6.4, 6.7",
		Run:     checkManifestCapabilityBounds,
	},
}

func init() {
	checks = append(checks, banChecks...)
}
