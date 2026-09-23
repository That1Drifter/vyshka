package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// banPlugin is a hand-driven plugin for the bans stage's mock: it reads pages
// of the list the way a candidate would and reports what it chooses to.
type banPlugin struct {
	*testPlugin
	seq int64
}

func newBanPlugin(t *testing.T, h *mockHub) *banPlugin {
	t.Helper()
	return &banPlugin{testPlugin: newTestPlugin(t, h)}
}

func (p *banPlugin) report(revision int64) {
	p.t.Helper()
	p.seq++
	p.poll(typedEnvelope("applied-"+time.Now().Format("150405.000000000"), p.seq, "bans.applied",
		map[string]any{"revision": revision}))
}

// page reads one page on the GET spelling and returns its status and body.
func (p *banPlugin) page(cursor string) (int, map[string]any) {
	p.t.Helper()
	target := p.baseURL + "/plugin/v1/bans"
	if cursor != "" {
		target += "?cursor=" + cursor
	}
	request, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		p.t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+p.sessionToken)
	response, err := p.client.Do(request)
	if err != nil {
		p.t.Fatal(err)
	}
	defer response.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(response.Body).Decode(&body)
	return response.StatusCode, body
}

// walk reads the list from the first page to the last.
func (p *banPlugin) walk() (int64, int) {
	p.t.Helper()
	cursor := ""
	var revision int64 = -1
	entries := 0
	for pages := 0; pages < 100; pages++ {
		status, body := p.page(cursor)
		if status != http.StatusOK {
			p.t.Fatalf("a page answered %d: %v", status, body)
		}
		served := int64(body["revision"].(float64))
		if revision < 0 {
			revision = served
		} else if served != revision {
			p.t.Fatalf("a walk of %d was served a page at %d", revision, served)
		}
		entries += len(body["bans"].([]any))
		next, _ := body["nextCursor"].(string)
		if next == "" {
			return revision, entries
		}
		cursor = next
	}
	p.t.Fatal("the walk did not end")
	return 0, 0
}

// A report of a revision the plugin never read whole is faulted (section
// 13.4); one after a whole walk is not.
func TestBanReportWithoutAWalkIsFaulted(t *testing.T) {
	h, err := startMockHub("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	p := newBanPlugin(t, h)
	h.mu.Lock()
	h.setBanListLocked(10, banEntries("t", 5))
	h.mu.Unlock()

	p.report(10)
	if faults := faultMessages(h); !strings.Contains(faults, "without having read every page") {
		t.Fatalf("a report without a walk was not faulted; recorded faults:\n%s", faults)
	}

	h.mu.Lock()
	h.faults = nil
	h.mu.Unlock()
	// Half a walk is not a walk.
	if status, _ := p.page(""); status != http.StatusOK {
		t.Fatalf("first page answered %d", status)
	}
	p.report(10)
	if faults := faultMessages(h); !strings.Contains(faults, "without having read every page") {
		t.Fatalf("a report after half a walk was not faulted; recorded faults:\n%s", faults)
	}

	h.mu.Lock()
	h.faults = nil
	h.mu.Unlock()
	revision, entries := p.walk()
	if revision != 10 || entries != 6 {
		t.Fatalf("the walk read %d entries at %d, want 6 at 10", entries, revision)
	}
	p.report(10)
	if faults := faultMessages(h); faults != "" {
		t.Fatalf("a report after a whole walk was faulted:\n%s", faults)
	}
}

// The mock serves a walk at the revision it began at while the list moves,
// and answers an armed conflict once (section 13.3).
func TestMockBanListPinsWalksAndArmsConflicts(t *testing.T) {
	held := banHoldBound
	banHoldBound = 100 * time.Millisecond
	t.Cleanup(func() { banHoldBound = held })
	h, err := startMockHub("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	p := newBanPlugin(t, h)
	h.mu.Lock()
	h.prepareBanListLocked(12, banEntries("m", 1))
	h.setBanListLocked(11, banEntries("n", 5))
	h.bans.bumpAfterFirstPage[11] = 12
	h.mu.Unlock()

	if revision, entries := p.walk(); revision != 11 || entries != 6 {
		t.Fatalf("a walk begun at 11 read %d entries at %d, want 6 at 11", entries, revision)
	}
	if revision, entries := p.walk(); revision != 12 || entries != 2 {
		t.Fatalf("the next walk read %d entries at %d, want 2 at 12", entries, revision)
	}

	h.mu.Lock()
	h.setBanListLocked(13, banEntries("o", 5))
	h.bans.conflictArmed = true
	h.mu.Unlock()
	_, first := p.page("")
	status, body := p.page(first["nextCursor"].(string))
	if status != http.StatusConflict {
		t.Fatalf("an armed conflict answered %d: %v", status, body)
	}
	// The conflict sticks to the walk it hit: the same cursor again is
	// refused again, so only a walk begun anew gets past it.
	if status, _ := p.page(first["nextCursor"].(string)); status != http.StatusConflict {
		t.Fatalf("the conflicted cursor sent again answered %d, want 409 again", status)
	}
	if revision, _ := p.walk(); revision != 13 {
		t.Fatalf("the walk after the conflict read revision %d, want 13", revision)
	}
	status, _ = p.page("r99o0w1")
	if status != http.StatusConflict {
		t.Errorf("a cursor at a revision above the current one answered %d, want 409", status)
	}
	status, _ = p.page("garbage")
	if status != http.StatusBadRequest || !strings.Contains(faultMessages(h), "never issued") {
		t.Errorf("a cursor the mock never issued answered %d and faulted %q", status, faultMessages(h))
	}
}

// Without the capability the stage has nothing to grade and says so; with it,
// a plugin that never walks fails, naming what it owed.
func TestBansStageGradesOnlyADeclaringPlugin(t *testing.T) {
	h, err := startMockHub("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	p := newTestPlugin(t, h)
	manifest := map[string]any{
		"game": "conformance", "manifestRevision": 1,
		"actions": []map[string]any{{"code": "test.echo", "context": "world"}},
	}
	p.poll(typedEnvelope("manifest-1", 1, "manifest.publish", manifest))
	results := runStages(&harness{hub: h, checkTimeout: 200 * time.Millisecond, banAllowance: 100 * time.Millisecond},
		[]Stage{bansStage})
	if len(results) != 1 || !results[0].Passed || !strings.Contains(results[0].Note, "does not declare the bans capability") {
		t.Fatalf("a plugin without the capability = %+v, want a pass with a note", results)
	}

	manifest["manifestRevision"] = 2
	manifest["capabilities"] = []string{"bans"}
	p.poll(typedEnvelope("manifest-2", 2, "manifest.publish", manifest))
	results = runStages(&harness{hub: h, checkTimeout: 200 * time.Millisecond, banAllowance: 100 * time.Millisecond},
		[]Stage{bansStage})
	if len(results) != 1 || results[0].Passed || !strings.Contains(results[0].Error, "a list of several pages announced by bans.changed") {
		t.Fatalf("a declaring plugin that never walks = %+v, want a failure naming the report it owed", results)
	}
}
