package testplugin_test

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/That1Drifter/vyshka/cmd/vyshka/internal/testplugin"
)

// countEvents walks every page of a server's feed for one type.
func (r *rig) countEvents(serverID, eventType string) int {
	r.t.Helper()
	count := 0
	cursor := ""
	for {
		query := url.Values{"type": {eventType}, "limit": {"500"}}
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		var page struct {
			Events     []struct{} `json:"events"`
			NextCursor string     `json:"nextCursor"`
		}
		if status := r.call(http.MethodGet, "/api/v1/servers/"+serverID+"/events?"+query.Encode(), nil, &page); status != http.StatusOK {
			r.t.Fatalf("list events: status %d", status)
		}
		count += len(page.Events)
		if page.NextCursor == "" {
			return count
		}
		cursor = page.NextCursor
	}
}

// More events than one poll may carry go out over several polls, each within
// the hub's per-poll budget, so the hub refuses none and none is lost. A
// refusal, should one happen, is visible rather than swallowed.
func TestEmitStaysWithinThePollEventBudget(t *testing.T) {
	r := newRig(t)
	p := r.start(nil)
	r.awaitManifest(p.ServerID())

	const total = 1200
	events := make([]testplugin.Event, total)
	for i := range events {
		events[i] = testplugin.Event{Type: "cli-test.beat", Data: map[string]any{"n": i}}
	}
	p.Emit(events...)
	waitFor(t, 15*time.Second, fmt.Sprintf("all %d events to be stored", total), func() bool {
		return r.countEvents(p.ServerID(), "cli-test.beat") == total
	})
	if rejected := p.EventRejections(); len(rejected) != 0 {
		t.Errorf("the hub rejected %d batches: %v", len(rejected), rejected)
	}
}
