package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The installation ban list (spec section 13) as the mock hub serves it, and
// the stage that grades a candidate declaring the bans capability: it walks
// the list a page at a time, reads one revision whole, reports only what it
// applied, starts over on a conflict, reports again on a session that begins
// current, and walks a revision lower than the one it holds.

// banPageCap is the most entries the mock serves per page, whatever limit the
// plugin asks for: a hub bounds the page and clamps rather than refusing
// (section 13.3), and a small cap makes every list of the stage a walk of
// several pages.
const banPageCap = 2

type mockBanEntry struct {
	ID        string  `json:"id"`
	Player    banWho  `json:"player"`
	Reason    string  `json:"reason"`
	Name      string  `json:"name"`
	ExpiresAt *string `json:"expiresAt"`
}

type banWho struct {
	Platform string `json:"platform"`
	ID       string `json:"id"`
}

// banReport is one bans.applied the plugin sent: the revision, the session
// it rode, and whether every entry of that revision had been served before it.
type banReport struct {
	Revision int64
	Session  int
	Complete bool
}

// mockBans is the list's state. revision is the current one; lists holds
// every revision the mock has had, so a walk begun at one is served at it to
// the end. served records which entries of each revision have gone out, which
// is the black-box evidence that a report was preceded by a whole walk.
type mockBans struct {
	revision int64
	lists    map[int64][]mockBanEntry
	served   map[int64]map[int]bool
	// firstPages counts the walks begun at each revision.
	firstPages map[int64]int
	// bumpAfterFirstPage moves the list to a new revision the moment a walk
	// of the keyed one has been served its first page, which is how the stage
	// lands a change in the middle of a walk.
	bumpAfterFirstPage map[int64]int64
	// conflictArmed answers the next request carrying a cursor with 409
	// conflict, as a hub that can no longer serve the cursor's revision does.
	conflictArmed bool
	conflicts     int
	reports       []banReport
}

func newMockBans() *mockBans {
	return &mockBans{
		lists:              map[int64][]mockBanEntry{0: {}},
		served:             map[int64]map[int]bool{},
		firstPages:         map[int64]int{},
		bumpAfterFirstPage: map[int64]int64{},
	}
}

// complete reports whether every entry of a revision has been served; an
// empty revision is complete once a walk of it has begun.
func (b *mockBans) complete(revision int64) bool {
	list, known := b.lists[revision]
	if !known {
		return false
	}
	if len(list) == 0 {
		return b.firstPages[revision] > 0
	}
	for index := range list {
		if !b.served[revision][index] {
			return false
		}
	}
	return true
}

// prepareBanListLocked stores list as a revision, sorted the way a hub serves
// it (section 13.3), without making it current.
func (h *mockHub) prepareBanListLocked(revision int64, list []mockBanEntry) {
	sorted := append([]mockBanEntry(nil), list...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Player.Platform != sorted[j].Player.Platform {
			return sorted[i].Player.Platform < sorted[j].Player.Platform
		}
		return sorted[i].Player.ID < sorted[j].Player.ID
	})
	h.bans.lists[revision] = sorted
	h.bans.served[revision] = map[int]bool{}
}

// setBanListLocked makes list the current revision.
func (h *mockHub) setBanListLocked(revision int64, list []mockBanEntry) {
	h.prepareBanListLocked(revision, list)
	h.bans.revision = revision
	h.signalLocked()
}

// banCursor is the mock's cursor: the revision and the offset of the next
// entry, in the alphabet section 13.3 requires.
func banCursor(revision int64, offset int) string {
	return "r" + strconv.FormatInt(revision, 10) + "o" + strconv.Itoa(offset)
}

func parseBanCursor(value string) (int64, int, bool) {
	if !strings.HasPrefix(value, "r") {
		return 0, 0, false
	}
	revisionText, offsetText, found := strings.Cut(value[1:], "o")
	if !found {
		return 0, 0, false
	}
	revision, err := strconv.ParseInt(revisionText, 10, 64)
	if err != nil || revision < 0 {
		return 0, 0, false
	}
	offset, err := strconv.Atoi(offsetText)
	if err != nil || offset < 0 {
		return 0, 0, false
	}
	return revision, offset, true
}

// handleBans serves one page of the list on either spelling (section 13.3).
func (h *mockHub) handleBans(w http.ResponseWriter, r *http.Request) {
	h.abortIfSevered()
	inline, ok := h.errorMode(w, r)
	if !ok {
		return
	}
	token := bearer(r)
	query := r.URL.Query()

	h.mu.Lock()
	if token == "" || token != h.sessionToken || !h.sessionLive {
		if !h.issuedTokens[token] {
			h.faultLocked("5.3", "a ban list read carried a bearer token this harness never issued")
		}
		h.mu.Unlock()
		answer(w, inline, http.StatusUnauthorized, "session_invalid", "session is not live", nil)
		return
	}
	if limit := query.Get("limit"); limit != "" {
		if parsed, err := strconv.Atoi(limit); err != nil || parsed < 1 {
			h.faultLocked("13.3", "a ban list read asked for limit=%s; a limit is a positive integer", limit)
			h.mu.Unlock()
			answer(w, inline, http.StatusBadRequest, "bad_request", "limit must be a positive integer", nil)
			return
		}
	}
	revision, offset := h.bans.revision, 0
	cursor := query.Get("cursor")
	if cursor != "" {
		var ok bool
		revision, offset, ok = parseBanCursor(cursor)
		if !ok {
			h.faultLocked("13.3", "a ban list read carried cursor %q, which this harness never issued; a plugin sends back the nextCursor it was given, as it came", cursor)
			h.mu.Unlock()
			answer(w, inline, http.StatusBadRequest, "bad_request", "cursor is not one this hub issued", nil)
			return
		}
		if h.bans.conflictArmed || revision > h.bans.revision {
			if h.bans.conflictArmed {
				h.bans.conflictArmed = false
				h.bans.conflicts++
			}
			h.signalLocked()
			h.mu.Unlock()
			answer(w, inline, http.StatusConflict, "conflict",
				"this harness will not serve the revision the cursor names; start the walk over", nil)
			return
		}
	}
	list := h.bans.lists[revision]
	if offset > len(list) {
		offset = len(list)
	}
	end := min(offset+banPageCap, len(list))
	page := map[string]any{"revision": revision, "bans": append([]mockBanEntry{}, list[offset:end]...)}
	if end < len(list) {
		page["nextCursor"] = banCursor(revision, end)
	}
	if h.bans.served[revision] == nil {
		h.bans.served[revision] = map[int]bool{}
	}
	for index := offset; index < end; index++ {
		h.bans.served[revision][index] = true
	}
	if cursor == "" {
		h.bans.firstPages[revision]++
		if target, armed := h.bans.bumpAfterFirstPage[revision]; armed {
			delete(h.bans.bumpAfterFirstPage, revision)
			h.bans.revision = target
			h.queueBansChangedLocked(target)
		}
	}
	h.signalLocked()
	h.mu.Unlock()
	writeJSONBody(w, http.StatusOK, page)
}

// queueBansChangedLocked queues the section 13.3 nudge.
func (h *mockHub) queueBansChangedLocked(revision int64) {
	encoded, _ := json.Marshal(map[string]int64{"revision": revision})
	h.hubEnvCounter++
	h.outbound = append(h.outbound, &outboundItem{
		id:   fmt.Sprintf("conformance-hub-%d", h.hubEnvCounter),
		typ:  "bans.changed",
		ts:   time.Now().UTC().Format(time.RFC3339),
		body: encoded,
	})
}

// interpretBansAppliedLocked records one report and faults a revision the
// plugin cannot have applied: one it never walked whole (section 13.4).
func (h *mockHub) interpretBansAppliedLocked(envelope *inboundEnvelope) {
	var body struct {
		Revision *json.Number `json:"revision"`
	}
	decoder := json.NewDecoder(strings.NewReader(envelope.Body))
	decoder.UseNumber()
	if decoder.Decode(&body) != nil || body.Revision == nil {
		h.faultLocked("13.4", "a bans.applied body carried no revision")
		return
	}
	revision, err := body.Revision.Int64()
	if err != nil || revision < 0 {
		h.faultLocked("13.4", "a bans.applied body carried revision %s, not an integer of 0 or more", body.Revision.String())
		return
	}
	report := banReport{Revision: revision, Session: h.sessionOrdinal, Complete: h.bans.complete(revision)}
	h.bans.reports = append(h.bans.reports, report)
	if !report.Complete {
		h.faultLocked("13.4", "the plugin reported applying ban list revision %d without having read every page of it; a plugin reports only a revision it has applied whole", revision)
	}
}

// banEntries mints count entries under prefix, one of them on a platform the
// candidate does not serve, which it must carry through its walk and ignore.
func banEntries(prefix string, count int) []mockBanEntry {
	entries := make([]mockBanEntry, 0, count+1)
	for i := 0; i < count; i++ {
		entries = append(entries, mockBanEntry{
			ID:     fmt.Sprintf("%s-ban-%02d", prefix, i),
			Player: banWho{Platform: "steam", ID: fmt.Sprintf("7656119%s%02d", prefix, i)},
			Reason: "conformance: " + prefix, Name: "Player " + strconv.Itoa(i),
		})
	}
	later := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	entries = append(entries, mockBanEntry{
		ID:     prefix + "-ban-elsewhere",
		Player: banWho{Platform: "conformance-other-platform", ID: prefix + "-elsewhere"},
		Reason: "conformance: another game's platform", ExpiresAt: &later,
	})
	return entries
}

// awaitBanReport waits for a report of revision, on session when that is not
// zero, arriving after the report count at since.
func (h *harness) awaitBanReport(revision int64, session int, since int, why string) error {
	hub := h.hub
	budget := h.checkTimeout + h.banRetryAllowance()
	return hub.await(budget, fmt.Sprintf("a bans.applied of revision %d (%s)", revision, why), func() bool {
		for _, report := range hub.bans.reports[since:] {
			if report.Revision == revision && (session == 0 || report.Session == session) {
				return true
			}
		}
		return false
	})
}

// banRetryAllowance is how long past -check-timeout the stage waits for a
// report: a plugin retries a failed walk on a cadence of its own (reference
// DayZ: 30 s), and a walk refused opaquely looks like any other failure to
// it, so one retry is allowed for.
func (h *harness) banRetryAllowance() time.Duration {
	if h.banAllowance > 0 {
		return h.banAllowance
	}
	return 35 * time.Second
}

var bansStage = Stage{
	ID:      "bans.sync",
	Title:   "A plugin declaring bans walks the list whole and reports what it applied",
	Section: "13.3, 13.4",
	Run: func(h *harness) error {
		hub := h.hub
		capable := false
		hub.view(func() {
			if hub.manifest != nil {
				for _, capability := range hub.manifest.Capabilities {
					if capability == "bans" {
						capable = true
					}
				}
			}
		})
		if !capable {
			return ungraded{"the manifest does not declare the bans capability, so the installation ban list was not graded (section 6.7)"}
		}
		var since int
		mark := func() { hub.view(func() { since = len(hub.bans.reports) }) }

		// A list of several pages, announced by a nudge.
		mark()
		hub.mu.Lock()
		hub.setBanListLocked(10, banEntries("a", 5))
		hub.queueBansChangedLocked(10)
		hub.mu.Unlock()
		if err := h.awaitBanReport(10, 0, since, "a list of several pages announced by bans.changed"); err != nil {
			return fmt.Errorf("%w; a plugin declaring bans walks the list when the revision it learns differs from the one it enforces, and reports it once applied (section 13.4)", err)
		}

		// A change lands in the middle of a walk: the walk of 11 is served
		// at 11 to its end while the list moves to 12, and the plugin ends
		// up reporting 12.
		mark()
		hub.mu.Lock()
		hub.prepareBanListLocked(12, banEntries("c", 4))
		hub.setBanListLocked(11, banEntries("b", 5))
		hub.bans.bumpAfterFirstPage[11] = 12
		hub.queueBansChangedLocked(11)
		hub.mu.Unlock()
		if err := h.awaitBanReport(12, 0, since, "the list moved on while a walk was under way"); err != nil {
			return fmt.Errorf("%w; the list moved from 11 to 12 while the plugin was walking 11, a bans.changed of 12 followed, and the plugin walks again (section 13.4)", err)
		}

		// A conflict partway through a walk: the plugin starts over.
		mark()
		hub.mu.Lock()
		hub.setBanListLocked(13, banEntries("d", 5))
		hub.bans.conflictArmed = true
		hub.queueBansChangedLocked(13)
		hub.mu.Unlock()
		if err := h.awaitBanReport(13, 0, since, "after a conflict partway through the walk"); err != nil {
			return fmt.Errorf("%w; a 409 conflict on a cursor restarts the walk from the first page (sections 13.3 and 13.4)", err)
		}
		var conflicts int
		hub.view(func() { conflicts = hub.bans.conflicts })
		if conflicts != 1 {
			return fmt.Errorf("the harness armed a conflict for the walk of revision 13 and it was never met: the plugin applied 13 without following a cursor, over a list longer than one page")
		}

		// A new session that begins with the revision the plugin already
		// enforces: it reports again, so the hub learns what the server holds.
		mark()
		var ordinal int
		hub.killSession()
		if err := hub.await(h.checkTimeout+h.banRetryAllowance(), "a new session after the harness ended the old one", func() bool {
			return hub.sessionLive
		}); err != nil {
			return err
		}
		hub.view(func() { ordinal = hub.sessionOrdinal })
		if err := h.awaitBanReport(13, ordinal, since, "a session that began with the revision already enforced"); err != nil {
			return fmt.Errorf("%w; a plugin reports on a session that begins with the revision it already enforces (section 13.4)", err)
		}

		// A lower revision, as from a hub restored from a backup: differs is
		// enough to walk.
		mark()
		hub.mu.Lock()
		hub.setBanListLocked(5, banEntries("e", 3))
		hub.queueBansChangedLocked(5)
		hub.mu.Unlock()
		if err := h.awaitBanReport(5, 0, since, "a revision lower than the one enforced"); err != nil {
			return fmt.Errorf("%w; a plugin walks whenever the revision differs from the one it enforces, not only when it exceeds it (section 13.4)", err)
		}
		return nil
	},
}

func init() {
	stages = append(stages, bansStage)
}
