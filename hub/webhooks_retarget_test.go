package hub_test

import (
	"net/http"
	"testing"
	"time"
)

// A webhook's pending deliveries were rendered under the subscription as it
// stood and follow the URL wherever an edit points it, so an edit that moves
// the URL, and a replay, are covered for the type of what they would send
// (spec sections 11.2 and 11.5). Without this a token could narrow the filter
// to what it may read, move the target to an address it controls, and receive
// what it may not.
func TestRetargetAndReplayRequireCoverageOfWhatIsPending(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	receiver := newTestReceiver(t, http.StatusNoContent)
	elsewhere := newTestReceiver(t, http.StatusNoContent)

	created, session := enrolledSession(t, server, "webhook-retarget")
	serverID := created.Server.ID
	webhookID, _ := registerWebhook(t, server, map[string]any{
		"url": receiver.server.URL, "events": []string{"*"}, "serverIds": []string{serverID},
	})
	path := "/api/v1/webhooks/" + webhookID

	// Paused first, so what lands stays pending and nothing reaches a target
	// during the test: every assertion below is about authorization, not
	// delivery.
	if status := call(t, server, http.MethodPatch, path, testAdminToken,
		map[string]any{"paused": true}, nil); status != http.StatusOK {
		t.Fatalf("pause: status = %d, want 200", status)
	}
	sendEvent(t, server, serverID, session.SessionToken, "private-mod.secret", 1)
	sendEvent(t, server, serverID, session.SessionToken, "public-mod.notice", 2)

	var privateID, publicID string
	deadline := time.Now().Add(10 * time.Second)
	for privateID == "" || publicID == "" {
		if time.Now().After(deadline) {
			t.Fatalf("the two deliveries never queued: %+v", webhookDeliveries(t, server, webhookID))
		}
		for _, delivery := range webhookDeliveries(t, server, webhookID) {
			if delivery["state"] != "pending" {
				t.Fatalf("a paused webhook's delivery is %v, want pending", delivery["state"])
			}
			switch delivery["type"] {
			case "private-mod.secret":
				privateID = delivery["id"].(string)
			case "public-mod.notice":
				publicID = delivery["id"].(string)
			}
		}
		time.Sleep(50 * time.Millisecond)
	}

	narrow, _ := mintToken(t, server, "narrow editor",
		"webhooks:manage", "servers:read", "events:read:public-mod.*")

	// Narrowing the filter is within the token's grants: nothing pending
	// moves, and the resulting subscription is one it could have registered.
	if status := call(t, server, http.MethodPatch, path, narrow,
		map[string]any{"events": []string{"public-mod.*"}}, nil); status != http.StatusOK {
		t.Fatalf("narrowing the filter: status = %d, want 200", status)
	}
	// Moving the URL is not, while a delivery the token may not read is
	// pending: that body would follow the URL.
	if code := errorCode(t, server, http.MethodPatch, path, narrow,
		map[string]any{"url": elsewhere.server.URL}, http.StatusForbidden); code != "forbidden" {
		t.Errorf("retarget with a private delivery pending: code = %q, want forbidden", code)
	}
	// Neither is sending that body again by hand.
	if code := errorCode(t, server, http.MethodPost, path+"/deliveries/"+privateID+"/replay", narrow,
		nil, http.StatusForbidden); code != "forbidden" {
		t.Errorf("replay of a private delivery: code = %q, want forbidden", code)
	}
	// A delivery the token may read, it may replay.
	if status := call(t, server, http.MethodPost, path+"/deliveries/"+publicID+"/replay", narrow,
		nil, nil); status != http.StatusAccepted {
		t.Errorf("replay of a public delivery by a token that reads it: status = %d, want 202", status)
	}
	// The bootstrap credential covers everything and may retarget.
	if status := call(t, server, http.MethodPatch, path, testAdminToken,
		map[string]any{"url": elsewhere.server.URL}, nil); status != http.StatusOK {
		t.Fatalf("retarget by admin: status = %d, want 200", status)
	}

	// Still paused throughout, so nothing reached either target: the refusals
	// above were refusals of authorization, and the acceptances did not leak.
	time.Sleep(500 * time.Millisecond)
	if receiver.count() != 0 || elsewhere.count() != 0 {
		t.Fatalf("a paused webhook delivered %d and %d times during the test", receiver.count(), elsewhere.count())
	}
}
