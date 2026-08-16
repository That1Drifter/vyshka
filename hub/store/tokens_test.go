package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/That1Drifter/vyshka/hub/store"
)

func TestAdminTokenLookupHonoursRevocationAndExpiry(t *testing.T) {
	ctx := context.Background()
	st := migrated(t)

	live, err := st.CreateAdminToken(ctx, store.NewAdminToken{
		Name: "live", TokenHash: "hash-live", Scopes: []string{"servers:read"},
	})
	if err != nil {
		t.Fatalf("create live token: %v", err)
	}
	if live.ExpiresAt != nil {
		t.Errorf("a token minted with no TTL expires at %v", live.ExpiresAt)
	}

	expiring, err := st.CreateAdminToken(ctx, store.NewAdminToken{
		Name: "expiring", TokenHash: "hash-expiring", Scopes: []string{"servers:read"},
		TTL: -time.Minute,
	})
	if err != nil {
		t.Fatalf("create expiring token: %v", err)
	}
	// A negative TTL is not a lifetime, so the store treats it as no TTL at all
	// rather than minting something already dead.
	if expiring.ExpiresAt != nil {
		t.Errorf("a negative TTL produced an expiry of %v, want none", expiring.ExpiresAt)
	}

	if _, err := st.CreateAdminToken(ctx, store.NewAdminToken{
		Name: "past", TokenHash: "hash-past", Scopes: []string{"servers:read"},
		TTL: time.Millisecond,
	}); err != nil {
		t.Fatalf("create short token: %v", err)
	}
	time.Sleep(5 * time.Millisecond)

	if _, err := st.AdminTokenByHash(ctx, "hash-live"); err != nil {
		t.Errorf("look up a live token: %v", err)
	}
	if _, err := st.AdminTokenByHash(ctx, "hash-past"); !errors.Is(err, store.ErrTokenRevoked) {
		t.Errorf("look up an expired token = %v, want ErrTokenRevoked", err)
	}
	if _, err := st.AdminTokenByHash(ctx, "hash-unknown"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("look up an unknown token = %v, want ErrNotFound", err)
	}

	// Listing live tokens is what boot uses to decide whether generating a
	// bootstrap credential is still first-run behavior, so an expired token
	// must not keep the hub locked out of that path.
	liveTokens, err := st.LiveAdminTokens(ctx)
	if err != nil {
		t.Fatalf("list live tokens: %v", err)
	}
	if len(liveTokens) != 2 {
		t.Errorf("live tokens = %d, want the two that have not expired", len(liveTokens))
	}
	// The scopes come back with them, because deciding whether any of them can
	// still manage tokens means reading the grammar, not counting rows.
	for _, stored := range liveTokens {
		if len(stored.Scopes) == 0 {
			t.Errorf("live token %s came back with no scopes", stored.ID)
		}
	}

	if err := st.RevokeAdminToken(ctx, live.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := st.AdminTokenByHash(ctx, "hash-live"); !errors.Is(err, store.ErrTokenRevoked) {
		t.Errorf("look up a revoked token = %v, want ErrTokenRevoked", err)
	}

	// Revocation keeps its first timestamp, because that is the moment an
	// operator reading the log later is looking for.
	revoked, err := st.AdminToken(ctx, live.ID)
	if err != nil {
		t.Fatalf("read revoked token: %v", err)
	}
	first := *revoked.RevokedAt
	time.Sleep(2 * time.Millisecond)
	if err := st.RevokeAdminToken(ctx, live.ID); err != nil {
		t.Fatalf("second revoke: %v", err)
	}
	again, err := st.AdminToken(ctx, live.ID)
	if err != nil {
		t.Fatalf("read twice-revoked token: %v", err)
	}
	if !again.RevokedAt.Equal(first) {
		t.Errorf("revokedAt moved from %s to %s on a second revocation", first, again.RevokedAt)
	}

	if err := st.RevokeAdminToken(ctx, "no-such-token"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("revoke an unknown token = %v, want ErrNotFound", err)
	}
}

// The authorization scope of an action read depends on the action's own code,
// so the code has to be readable without touching the row. ActionByID applies
// lazy expiry, which is a write: going through it to answer "may this token see
// this action?" would let a token with no grant over the action drive a state
// change, on a route that is not even audited.
func TestActionCodeDoesNotExpireTheAction(t *testing.T) {
	ctx := context.Background()
	st := migrated(t)
	serverID := enrolledServer(t, st, "action code lookup")
	session := startSession(t, st, serverID, "token-hash")
	publishManifestRow(t, st, session.ID, 1)

	// Dispatched live, then left to lapse: DispatchAction itself finishes with
	// a full read, so an action born past its deadline would already be
	// expired before this test began. There is no sweeper here, so the only
	// thing that can expire it now is a read.
	dispatch(t, st, serverID, "action-1", "", 50*time.Millisecond, 10)
	time.Sleep(80 * time.Millisecond)

	code, err := st.ActionCode(ctx, "action-1")
	if err != nil {
		t.Fatalf("read action code: %v", err)
	}
	if code != "test.heal" {
		t.Errorf("code = %q, want test.heal", code)
	}

	var state string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT state FROM actions WHERE id = ?`, "action-1").Scan(&state); err != nil {
		t.Fatalf("read state directly: %v", err)
	}
	if state != store.ActionQueued {
		t.Errorf("state = %q after a code lookup, want it untouched at %q; "+
			"the authorization path must not mutate the row", state, store.ActionQueued)
	}

	// The full read is still expected to expire it: that behavior belongs to
	// the authorized read, not to the authorization check.
	action, err := st.ActionByID(ctx, "action-1")
	if err != nil {
		t.Fatalf("read action: %v", err)
	}
	if action.State != store.ActionExpired {
		t.Errorf("state = %q after a full read, want %q", action.State, store.ActionExpired)
	}

	if _, err := st.ActionCode(ctx, "no-such-action"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("code of an unknown action = %v, want ErrNotFound", err)
	}
}

func TestAuditRecordsQueryAndPrune(t *testing.T) {
	ctx := context.Background()
	st := migrated(t)

	serverID := enrolledServer(t, st, "audited server")
	for i := range 5 {
		if _, err := st.RecordAudit(ctx, store.NewAuditRecord{
			TokenID: "token-a", TokenName: "a", Method: "POST",
			Path: "/api/v1/servers/" + serverID + "/actions", Status: 202,
			SourceIP: "127.0.0.1", PayloadDigest: "digest", ServerID: serverID,
			Detail: json.RawMessage(`{"code":"example-mod.heal"}`), Retention: time.Hour,
		}); err != nil {
			t.Fatalf("record audit %d: %v", i, err)
		}
	}
	for range 2 {
		if _, err := st.RecordAudit(ctx, store.NewAuditRecord{
			TokenID: "token-b", TokenName: "b", Method: "DELETE",
			Path: "/api/v1/tokens/x", Status: 204, SourceIP: "10.0.0.1",
			Retention: time.Hour,
		}); err != nil {
			t.Fatalf("record audit: %v", err)
		}
	}
	// Two on a retention short enough to be past by the time the prune runs.
	for range 2 {
		if _, err := st.RecordAudit(ctx, store.NewAuditRecord{
			TokenID: "token-a", TokenName: "a", Method: "POST", Path: "/api/v1/servers",
			Status: 201, SourceIP: "127.0.0.1", Retention: time.Millisecond,
		}); err != nil {
			t.Fatalf("record stale audit: %v", err)
		}
	}
	time.Sleep(5 * time.Millisecond)

	all, err := st.AuditRecords(ctx, store.AuditQuery{Limit: 50})
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	if len(all) != 9 {
		t.Fatalf("read %d records, want 9", len(all))
	}
	// Newest first, and strictly ordered even when rows share a millisecond.
	for i := 1; i < len(all); i++ {
		previous, current := all[i-1], all[i]
		if previous.At.Before(current.At) ||
			(previous.At.Equal(current.At) && previous.ID <= current.ID) {
			t.Fatalf("records %d and %d are out of order: %s/%s then %s/%s",
				i-1, i, previous.At, previous.ID, current.At, current.ID)
		}
	}

	byToken, err := st.AuditRecords(ctx, store.AuditQuery{TokenID: "token-b", Limit: 50})
	if err != nil {
		t.Fatalf("read audit by token: %v", err)
	}
	if len(byToken) != 2 {
		t.Errorf("token-b has %d records, want 2", len(byToken))
	}

	byServer, err := st.AuditRecords(ctx, store.AuditQuery{ServerID: serverID, Limit: 50})
	if err != nil {
		t.Fatalf("read audit by server: %v", err)
	}
	if len(byServer) != 5 {
		t.Errorf("the server has %d records, want 5", len(byServer))
	}

	pruned, err := st.PruneAudit(ctx, 100)
	if err != nil {
		t.Fatalf("prune audit: %v", err)
	}
	if pruned != 2 {
		t.Errorf("pruned %d records, want the 2 past retention", pruned)
	}
	remaining, err := st.AuditRecords(ctx, store.AuditQuery{Limit: 50})
	if err != nil {
		t.Fatalf("read audit after prune: %v", err)
	}
	if len(remaining) != 7 {
		t.Errorf("%d records survived the prune, want 7", len(remaining))
	}
}

// A non-positive limit must not be read by SQLite as "no limit": the one call
// that promises to be bounded would become the unbounded delete it exists to
// avoid.
func TestPruneAuditClampsANonPositiveLimit(t *testing.T) {
	ctx := context.Background()
	st := migrated(t)

	for range 3 {
		if _, err := st.RecordAudit(ctx, store.NewAuditRecord{
			TokenName: "a", Method: "POST", Path: "/api/v1/servers", Status: 201,
			Retention: time.Millisecond,
		}); err != nil {
			t.Fatalf("record audit: %v", err)
		}
	}
	time.Sleep(5 * time.Millisecond)

	pruned, err := st.PruneAudit(ctx, 0)
	if err != nil {
		t.Fatalf("prune with a zero limit: %v", err)
	}
	if pruned != 3 {
		t.Errorf("pruned %d records, want all 3: a clamped limit must still be a working bound", pruned)
	}
}

// Retention defaults rather than stamping a record as already expired when a
// caller passes nothing, so a misconfigured hub keeps its log instead of
// deleting it as fast as it is written.
func TestAuditRetentionDefaultsWhenUnset(t *testing.T) {
	ctx := context.Background()
	st := migrated(t)

	if _, err := st.RecordAudit(ctx, store.NewAuditRecord{
		TokenName: "a", Method: "POST", Path: "/api/v1/servers", Status: 201,
	}); err != nil {
		t.Fatalf("record audit: %v", err)
	}
	pruned, err := st.PruneAudit(ctx, 10)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if pruned != 0 {
		t.Errorf("pruned %d records written with no retention, want 0", pruned)
	}
}
