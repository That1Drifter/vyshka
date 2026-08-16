package hub_test

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/That1Drifter/vyshka/hub"
)

// manifestRecord mirrors the Admin API manifest view on the wire.
type manifestRecord struct {
	Revision    int64  `json:"revision"`
	PublishedAt string `json:"publishedAt"`
	Manifest    struct {
		ManifestRevision int64 `json:"manifestRevision"`
		Actions          []struct {
			Code string `json:"code"`
			Name string `json:"name"`
		} `json:"actions"`
	} `json:"manifest"`
}

// healManifest is the demo manifest of spec section 6: one heal action with a
// params schema inside the subset.
func healManifest(revision int64, actions ...map[string]any) map[string]any {
	if actions == nil {
		actions = []map[string]any{{
			"code": "example-mod.heal", "name": "Heal player",
			"context": "player", "namespace": "example-mod", "danger": "warning",
			"params": map[string]any{
				"type":     "object",
				"required": []string{"amount"},
				"properties": map[string]any{
					"amount": map[string]any{"type": "integer", "minimum": 1, "maximum": 100},
					"item":   map[string]any{"type": "string", "x-vyshka-widget": "itemlist"},
				},
			},
		}}
	}
	return map[string]any{
		"game":             "test-game",
		"plugin":           map[string]any{"name": "test-plugin", "version": "1.0.0"},
		"manifestRevision": revision,
		"actions":          actions,
		"contexts":         []any{},
		"events":           []any{},
	}
}

// publishEnvelope frames one manifest.publish envelope.
func publishEnvelope(seq int64, body map[string]any) map[string]any {
	return map[string]any{
		"v": 1, "id": "manifest-publish-" + strconv.FormatInt(seq, 10),
		"type": "manifest.publish", "seq": seq,
		"ts": time.Now().UTC().Format(time.RFC3339), "body": body,
	}
}

func getManifest(t *testing.T, server *hub.Server, serverID string) manifestRecord {
	t.Helper()
	var record manifestRecord
	status := call(t, server, http.MethodGet, "/api/v1/servers/"+serverID+"/manifest",
		testAdminToken, nil, &record)
	if status != http.StatusOK {
		t.Fatalf("get manifest: status = %d, want 200", status)
	}
	return record
}

func TestManifestPublishAndAdminRead(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	created, live := enrolledSession(t, server, "manifest publish")

	result := pollNow(t, server, created.Server.ID, live.SessionToken, map[string]any{
		"envelopes": []map[string]any{publishEnvelope(1, healManifest(1))},
	})
	if result.Ack != 1 {
		t.Fatalf("ack = %d after manifest.publish, want 1", result.Ack)
	}

	record := getManifest(t, server, created.Server.ID)
	if record.Revision != 1 {
		t.Errorf("revision = %d, want 1", record.Revision)
	}
	if _, err := time.Parse(time.RFC3339, record.PublishedAt); err != nil {
		t.Errorf("publishedAt %q is not RFC 3339: %v", record.PublishedAt, err)
	}
	if record.Manifest.ManifestRevision != 1 {
		t.Errorf("the stored body's manifestRevision = %d, want the published 1",
			record.Manifest.ManifestRevision)
	}
	if len(record.Manifest.Actions) != 1 || record.Manifest.Actions[0].Code != "example-mod.heal" {
		t.Errorf("stored actions = %+v, want the published heal action", record.Manifest.Actions)
	}
}

// Runtime republish: changing a mod's actions costs one message, not a server
// restart, and the revision gate keeps ordering sane (spec section 6.1).
func TestManifestRevisionMonotonicity(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	created, live := enrolledSession(t, server, "manifest revisions")

	renamed := healManifest(2, map[string]any{"code": "example-mod.revive", "name": "Revive player"})

	// Revision 1, then a runtime republish at revision 2, on the same session.
	pollNow(t, server, created.Server.ID, live.SessionToken, map[string]any{
		"envelopes": []map[string]any{publishEnvelope(1, healManifest(1))},
	})
	pollNow(t, server, created.Server.ID, live.SessionToken, map[string]any{
		"envelopes": []map[string]any{publishEnvelope(2, renamed)},
	})
	record := getManifest(t, server, created.Server.ID)
	if record.Revision != 2 {
		t.Fatalf("revision = %d after a runtime republish, want 2", record.Revision)
	}
	if len(record.Manifest.Actions) != 1 || record.Manifest.Actions[0].Code != "example-mod.revive" {
		t.Fatalf("actions = %+v, want the revision 2 action set", record.Manifest.Actions)
	}

	// A lower revision is ignored; so is an equal one, whatever its content.
	lower := pollNow(t, server, created.Server.ID, live.SessionToken, map[string]any{
		"envelopes": []map[string]any{publishEnvelope(3, healManifest(1))},
	})
	if lower.Ack != 3 {
		t.Fatalf("ack = %d, want 3: an ignored revision is still acked", lower.Ack)
	}
	equal := healManifest(2, map[string]any{"code": "example-mod.other"})
	pollNow(t, server, created.Server.ID, live.SessionToken, map[string]any{
		"envelopes": []map[string]any{publishEnvelope(4, equal)},
	})

	record = getManifest(t, server, created.Server.ID)
	if record.Revision != 2 {
		t.Errorf("revision = %d after lower and equal republishes, want it held at 2", record.Revision)
	}
	if len(record.Manifest.Actions) != 1 || record.Manifest.Actions[0].Code != "example-mod.revive" {
		t.Errorf("actions = %+v changed on an equal-revision republish", record.Manifest.Actions)
	}
}

// An invalid manifest is rejected without dropping the session: the envelope is
// acked, the stored manifest is untouched, and a manifest.reject names what was
// wrong (spec section 6.4).
func TestManifestRejectionKeepsTheSessionAndTheStoredManifest(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	created, live := enrolledSession(t, server, "manifest rejection")

	pollNow(t, server, created.Server.ID, live.SessionToken, map[string]any{
		"envelopes": []map[string]any{publishEnvelope(1, healManifest(1))},
	})

	// Revision 2 uses a keyword outside the subset. The poll itself must
	// succeed, ack the envelope, and hand back the rejection notice.
	invalid := healManifest(2, map[string]any{
		"code": "example-mod.heal",
		"params": map[string]any{
			"type":       "object",
			"properties": map[string]any{"item": map[string]any{"type": "string", "pattern": "^a"}},
		},
	})
	result := poll(t, server, live.SessionToken, map[string]any{
		"envelopes": []map[string]any{publishEnvelope(2, invalid)},
	})
	if result.Ack != 2 {
		t.Fatalf("ack = %d, want 2: a rejected manifest is still acked", result.Ack)
	}

	var reject *wireEnvelope
	for i := range result.Envelopes {
		if result.Envelopes[i].Type == "manifest.reject" {
			reject = &result.Envelopes[i]
		}
	}
	if reject == nil {
		t.Fatalf("no manifest.reject in the poll response, got %+v", result.Envelopes)
	}
	var body struct {
		EnvelopeID       string `json:"envelopeId"`
		ManifestRevision int64  `json:"manifestRevision"`
		Errors           []struct {
			Path    string `json:"path"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(reject.Body, &body); err != nil {
		t.Fatalf("decode manifest.reject body %q: %v", reject.Body, err)
	}
	if body.EnvelopeID != "manifest-publish-2" {
		t.Errorf("envelopeId = %q, want the rejected manifest-publish-2", body.EnvelopeID)
	}
	if body.ManifestRevision != 2 {
		t.Errorf("manifestRevision = %d, want the rejected 2", body.ManifestRevision)
	}
	if len(body.Errors) == 0 {
		t.Fatal("manifest.reject carried no errors")
	}
	if !strings.Contains(body.Errors[0].Path, "actions[0].params") ||
		!strings.Contains(body.Errors[0].Path+body.Errors[0].Message, "pattern") {
		t.Errorf("fault %+v does not point at the unsupported pattern keyword", body.Errors[0])
	}

	// The stored manifest is still revision 1, and the session still works.
	record := getManifest(t, server, created.Server.ID)
	if record.Revision != 1 {
		t.Errorf("revision = %d after a rejected publish, want the accepted 1", record.Revision)
	}

	// The stored revision advances only on acceptance, so the corrected
	// manifest lands at the very revision that was just rejected.
	pollNow(t, server, created.Server.ID, live.SessionToken, map[string]any{
		"envelopes": []map[string]any{publishEnvelope(3, healManifest(2))},
	})
	if record := getManifest(t, server, created.Server.ID); record.Revision != 2 {
		t.Errorf("revision = %d after the corrected republish, want 2", record.Revision)
	}
}

// A retransmitted manifest.publish is a duplicate: acked again, applied once,
// and never answered with a second rejection.
func TestManifestDuplicateDeliveryIsIdempotent(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	created, live := enrolledSession(t, server, "manifest duplicates")

	invalid := healManifest(1, map[string]any{
		"code":   "example-mod.heal",
		"params": map[string]any{"minLength": 3},
	})
	envelope := publishEnvelope(1, invalid)

	first := poll(t, server, live.SessionToken, map[string]any{
		"envelopes": []map[string]any{envelope},
	})
	rejects := 0
	for _, delivered := range first.Envelopes {
		if delivered.Type == "manifest.reject" {
			rejects++
		}
	}
	if rejects != 1 {
		t.Fatalf("got %d manifest.reject envelopes, want 1", rejects)
	}

	// The plugin re-sends the same envelope (a retransmission), acking the
	// reject it already got. No new rejection may appear.
	second := pollNow(t, server, created.Server.ID, live.SessionToken, map[string]any{
		"ack":       first.Envelopes[len(first.Envelopes)-1].Seq,
		"envelopes": []map[string]any{envelope},
	})
	for _, delivered := range second.Envelopes {
		if delivered.Type == "manifest.reject" {
			t.Errorf("a duplicate delivery produced a second manifest.reject")
		}
	}
	if second.Ack != 1 {
		t.Errorf("ack = %d after the duplicate, want it held at 1", second.Ack)
	}
}

func TestManifestAdminReadErrors(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	created := createServer(t, server, "manifest errors", "test-game")

	// A server that never published has no manifest to read.
	if code := errorCode(t, server, http.MethodGet, "/api/v1/servers/"+created.Server.ID+"/manifest",
		testAdminToken, nil, http.StatusNotFound); code != "not_found" {
		t.Errorf("unpublished: error code = %q, want not_found", code)
	}
	if code := errorCode(t, server, http.MethodGet, "/api/v1/servers/nope/manifest",
		testAdminToken, nil, http.StatusNotFound); code != "not_found" {
		t.Errorf("unknown server: error code = %q, want not_found", code)
	}
	if code := errorCode(t, server, http.MethodGet, "/api/v1/servers/"+created.Server.ID+"/manifest",
		"", nil, http.StatusUnauthorized); code != "unauthorized" {
		t.Errorf("no admin token: error code = %q, want unauthorized", code)
	}
}

// The raw queue endpoint must refuse the manifest family: a forged
// manifest.reject would read as the hub's own word (spec section 5.5).
func TestQueueEnvelopeRefusesManifestTypes(t *testing.T) {
	t.Parallel()
	server := newTestServer(t)
	created := createServer(t, server, "manifest reserved", "test-game")
	path := "/api/v1/servers/" + created.Server.ID + "/envelopes"

	for _, reserved := range []string{"manifest.publish", "manifest.reject"} {
		if code := errorCode(t, server, http.MethodPost, path, testAdminToken,
			map[string]any{"type": reserved}, http.StatusConflict); code != "conflict" {
			t.Errorf("%s: error code = %q, want conflict", reserved, code)
		}
	}
}
