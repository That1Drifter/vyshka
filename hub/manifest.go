package hub

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/That1Drifter/vyshka/hub/internal/schema"
	"github.com/That1Drifter/vyshka/hub/store"
)

// Envelope types the manifest exchange uses (spec section 6).
const (
	envelopeTypeManifestPublish = "manifest.publish"
	envelopeTypeManifestReject  = "manifest.reject"
)

// Manifest content limits. The counts bound what one manifest may declare; the
// lengths mirror the caps the rest of the protocol already applies to names
// and types.
const (
	maxManifestActions  = 500
	maxManifestContexts = 100
	maxManifestEvents   = 500
	maxCodeLength       = 128
	maxLabelLength      = 200
	maxNamespaceLength  = 64

	// maxManifestFaults caps how many faults a manifest.reject echoes back. A
	// manifest wrong in more ways than this is wrong enough to fix iteratively.
	maxManifestFaults = 20
)

// dangerLevels are the advisory `danger` values of spec section 6.1. Empty
// means the plugin declared nothing, which reads as `none`.
var dangerLevels = map[string]bool{"": true, "none": true, "warning": true, "destructive": true}

// maxManifestRevision is 2^53 - 1. Revisions ride through JSON tooling that
// reads numbers as float64, so a revision such tooling would round is refused
// rather than compared inexactly (see schema.maxExactNumber for the same rule
// on schema constants).
const maxManifestRevision = int64(1)<<53 - 1

// manifestBody is the manifest.publish body (spec section 6). Unknown fields
// are tolerated per section 2.1. The advisory fields are modelled even though
// the hub never routes on them, so a body that gives them the wrong JSON type
// is rejected instead of stored verbatim as something the companion schema
// calls invalid. A JSON null anywhere an optional field may appear reads as
// the field being absent (section 6.4).
type manifestBody struct {
	Game             string            `json:"game"`
	Plugin           *manifestPlugin   `json:"plugin"`
	ManifestRevision *int64            `json:"manifestRevision"`
	Actions          []manifestAction  `json:"actions"`
	Contexts         []manifestContext `json:"contexts"`
	Events           []manifestEvent   `json:"events"`
}

type manifestPlugin struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type manifestAction struct {
	Code      string          `json:"code"`
	Name      string          `json:"name"`
	Context   string          `json:"context"`
	Namespace string          `json:"namespace"`
	Danger    string          `json:"danger"`
	Params    json.RawMessage `json:"params"`
}

type manifestContext struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

type manifestEvent struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Namespace string          `json:"namespace"`
	Payload   json.RawMessage `json:"payload"`
}

// validateManifest checks a manifest.publish body against section 6. It
// returns the revision and no faults, or the faults that reject it. The body
// itself is what gets stored on acceptance, verbatim.
func validateManifest(body json.RawMessage) (int64, []schema.Fault) {
	if len(body) == 0 {
		return 0, []schema.Fault{{Path: "", Message: "body is required"}}
	}
	var manifest manifestBody
	if err := json.Unmarshal(body, &manifest); err != nil {
		return 0, []schema.Fault{{Path: "", Message: "body does not match the manifest shape: " + err.Error()}}
	}

	var faults []schema.Fault
	fault := func(path, format string, args ...any) {
		faults = append(faults, schema.Fault{Path: path, Message: fmt.Sprintf(format, args...)})
	}
	// tooLong applies the length limits in Unicode code points, the unit the
	// companion schema's maxLength speaks, not in bytes.
	tooLong := func(value string, limit int) bool {
		return utf8.RuneCountInString(value) > limit
	}

	var revision int64
	switch {
	case manifest.ManifestRevision == nil:
		fault("manifestRevision", "manifestRevision is required")
	case *manifest.ManifestRevision < 1:
		fault("manifestRevision", "manifestRevision must be 1 or above, got %d", *manifest.ManifestRevision)
	case *manifest.ManifestRevision > maxManifestRevision:
		fault("manifestRevision", "manifestRevision must be below 2^53")
	default:
		revision = *manifest.ManifestRevision
	}

	if tooLong(manifest.Game, maxNamespaceLength) {
		fault("game", "game is longer than %d characters", maxNamespaceLength)
	}
	if manifest.Plugin != nil {
		if tooLong(manifest.Plugin.Name, maxLabelLength) {
			fault("plugin.name", "name is longer than %d characters", maxLabelLength)
		}
		if tooLong(manifest.Plugin.Version, maxLabelLength) {
			fault("plugin.version", "version is longer than %d characters", maxLabelLength)
		}
	}

	// A count over its cap faults once and skips the per-item pass: a manifest
	// that oversized is rejected either way, and validating a quarter million
	// items to say so in more detail would let the sender buy CPU with one
	// fault (the notice is capped at maxManifestFaults regardless).
	if len(manifest.Actions) > maxManifestActions {
		fault("actions", "a manifest may declare at most %d actions, got %d",
			maxManifestActions, len(manifest.Actions))
		manifest.Actions = nil
	}
	codes := make(map[string]bool, len(manifest.Actions))
	for i, action := range manifest.Actions {
		path := fmt.Sprintf("actions[%d]", i)
		switch {
		case action.Code == "":
			fault(path+".code", "code is required")
		case tooLong(action.Code, maxCodeLength):
			fault(path+".code", "code is longer than %d characters", maxCodeLength)
		case codes[action.Code]:
			fault(path+".code", "code %q appears more than once; codes are unique within a server", action.Code)
		default:
			codes[action.Code] = true
		}
		if tooLong(action.Name, maxLabelLength) {
			fault(path+".name", "name is longer than %d characters", maxLabelLength)
		}
		if tooLong(action.Context, maxNamespaceLength) {
			fault(path+".context", "context is longer than %d characters", maxNamespaceLength)
		}
		if tooLong(action.Namespace, maxNamespaceLength) {
			fault(path+".namespace", "namespace is longer than %d characters", maxNamespaceLength)
		}
		if !dangerLevels[action.Danger] {
			fault(path+".danger", "danger must be none, warning, or destructive, got %q", action.Danger)
		}
		faults = append(faults, compileParams(action.Params, path+".params")...)
	}

	if len(manifest.Contexts) > maxManifestContexts {
		fault("contexts", "a manifest may declare at most %d contexts, got %d",
			maxManifestContexts, len(manifest.Contexts))
		manifest.Contexts = nil
	}
	contextIDs := make(map[string]bool, len(manifest.Contexts))
	for i, declared := range manifest.Contexts {
		path := fmt.Sprintf("contexts[%d]", i)
		switch {
		case declared.ID == "":
			fault(path+".id", "id is required")
		case tooLong(declared.ID, maxNamespaceLength):
			fault(path+".id", "id is longer than %d characters", maxNamespaceLength)
		case contextIDs[declared.ID]:
			fault(path+".id", "id %q appears more than once", declared.ID)
		default:
			contextIDs[declared.ID] = true
		}
		if tooLong(declared.Name, maxLabelLength) {
			fault(path+".name", "name is longer than %d characters", maxLabelLength)
		}
		if tooLong(declared.Namespace, maxNamespaceLength) {
			fault(path+".namespace", "namespace is longer than %d characters", maxNamespaceLength)
		}
	}

	if len(manifest.Events) > maxManifestEvents {
		fault("events", "a manifest may declare at most %d events, got %d",
			maxManifestEvents, len(manifest.Events))
		manifest.Events = nil
	}
	eventIDs := make(map[string]bool, len(manifest.Events))
	for i, declared := range manifest.Events {
		path := fmt.Sprintf("events[%d]", i)
		switch {
		case declared.ID == "":
			fault(path+".id", "id is required")
		case tooLong(declared.ID, maxCodeLength):
			fault(path+".id", "id is longer than %d characters", maxCodeLength)
		case eventIDs[declared.ID]:
			fault(path+".id", "id %q appears more than once", declared.ID)
		default:
			eventIDs[declared.ID] = true
		}
		if tooLong(declared.Name, maxLabelLength) {
			fault(path+".name", "name is longer than %d characters", maxLabelLength)
		}
		if tooLong(declared.Namespace, maxNamespaceLength) {
			fault(path+".namespace", "namespace is longer than %d characters", maxNamespaceLength)
		}
		faults = append(faults, compileParams(declared.Payload, path+".payload")...)
	}

	if len(faults) > 0 {
		return 0, faults
	}
	return revision, nil
}

// compileParams runs the schema-subset compiler over an optional schema field
// and rebases its fault paths into the manifest.
func compileParams(raw json.RawMessage, path string) []schema.Fault {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	_, faults := schema.Compile(raw)
	for i, one := range faults {
		if one.Path == "" {
			faults[i].Path = path
		} else {
			faults[i].Path = path + "." + one.Path
		}
	}
	return faults
}

// preparedManifest is one manifest.publish envelope after body validation:
// either a publish to apply or the rejection notice that answers it.
type preparedManifest struct {
	publish *store.ManifestPublish
	reject  store.Notice
}

// prepareManifests validates every manifest.publish body in a batch, keyed by
// batch index. Validation runs before classification because it depends only
// on content; whether a prepared outcome is applied is decided later, when
// classification says which envelopes are newly accepted.
//
// A duplicate delivery therefore pays for validation again before being
// discarded. That is the cheaper side of the trade: the work is bounded by the
// request-size cap the poll already parses, while validating lazily inside the
// classify callback would put CPU-bound work into the store's transaction,
// which on SQLite's single connection is the whole database's critical
// section.
func prepareManifests(envelopes []inboundEnvelope) map[int]preparedManifest {
	var prepared map[int]preparedManifest
	for index, e := range envelopes {
		if e.Type != envelopeTypeManifestPublish {
			continue
		}
		if prepared == nil {
			prepared = make(map[int]preparedManifest)
		}
		if revision, faults := validateManifest(e.Body); len(faults) == 0 {
			prepared[index] = preparedManifest{
				publish: &store.ManifestPublish{Revision: revision, Body: e.Body},
			}
		} else {
			prepared[index] = preparedManifest{reject: newManifestReject(e.ID, e.Body, faults)}
		}
	}
	return prepared
}

// manifestRejectBody is what a hub sends back for a manifest it refused
// (spec section 6.4).
type manifestRejectBody struct {
	// EnvelopeID names the manifest.publish envelope this rejection answers.
	EnvelopeID string `json:"envelopeId"`
	// ManifestRevision is the refused revision, present whenever it was
	// readable from the rejected body: a pointer, because a readable 0 or
	// negative revision (itself a reason for the rejection) must still be
	// echoed rather than clamped or omitted.
	ManifestRevision *int64 `json:"manifestRevision,omitempty"`
	// Errors is capped at maxManifestFaults entries.
	Errors []schema.Fault `json:"errors"`
}

// newManifestReject builds the manifest.reject notice for one refused publish.
func newManifestReject(envelopeID string, body json.RawMessage, faults []schema.Fault) store.Notice {
	// The refused revision is best-effort: the body already failed validation,
	// but a readable revision helps the plugin match the rejection to what it
	// sent. Unreadable (absent, or not an integer) means omitted.
	var revision struct {
		ManifestRevision *int64 `json:"manifestRevision"`
	}
	if err := json.Unmarshal(body, &revision); err != nil {
		// The decoder allocates the pointer before a type mismatch fails the
		// store, so an errored decode must not be mistaken for a readable 0.
		revision.ManifestRevision = nil
	}

	if len(faults) > maxManifestFaults {
		faults = faults[:maxManifestFaults]
	}
	encoded, err := json.Marshal(manifestRejectBody{
		EnvelopeID:       envelopeID,
		ManifestRevision: revision.ManifestRevision,
		Errors:           faults,
	})
	if err != nil {
		// Faults are strings all the way down; this cannot happen, and a
		// degraded notice beats a dropped one if it somehow does.
		degraded, _ := json.Marshal(manifestRejectBody{
			EnvelopeID: envelopeID,
			Errors:     []schema.Fault{{Path: "", Message: "the manifest was rejected"}},
		})
		encoded = degraded
	}
	return store.Notice{Type: envelopeTypeManifestReject, Body: encoded}
}

// manifestView is the Admin API representation of a stored manifest: the
// accepted body verbatim, with the hub's metadata beside it rather than
// rewritten into it.
type manifestView struct {
	Revision    int64           `json:"revision"`
	PublishedAt time.Time       `json:"publishedAt"`
	Manifest    json.RawMessage `json:"manifest"`
}

// handleGetManifest answers with a server's stored manifest (spec section 6.5).
func (s *Server) handleGetManifest(w http.ResponseWriter, r *http.Request) {
	server, ok := s.lookupServer(w, r)
	if !ok {
		return
	}

	manifest, err := s.store.Manifest(r.Context(), server.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, codeNotFound, "this server has not published a manifest")
		return
	case err != nil:
		s.writeInternalError(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, manifestView{
		Revision:    manifest.Revision,
		PublishedAt: manifest.PublishedAt,
		Manifest:    manifest.Body,
	})
}
