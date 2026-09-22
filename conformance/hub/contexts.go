package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Custom context enumeration, the hub half (spec section 6.2): the Admin API
// read that asks the plugin what a declared context holds, the
// context.enumerate that carries the question, and the answer handed back
// verbatim. Plus the `context` annotation of section 6.1, which a manifest
// may only put on a string schema and only for a context it declares.

// contextEntriesRecord is the Admin API enumeration view (spec section 6.2),
// hand-written like every other shape in this suite.
type contextEntriesRecord struct {
	Context      string            `json:"context"`
	Entries      []json.RawMessage `json:"entries"`
	Reason       *string           `json:"reason"`
	EnumeratedAt string            `json:"enumeratedAt"`
}

// contextManifest declares two custom contexts: one the plugin answers with
// entries, one it answers with nothing and a reason. The claim action's
// `neighbour` param draws on the first, which is the section 6.1 annotation.
func contextManifest(revision int64) map[string]any {
	body := manifestBody(revision, map[string]any{
		"code": "example-mod.claim", "name": "Claim territory", "context": "example-mod.territory",
		"namespace": "example-mod", "danger": "none",
		"params": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"neighbour": map[string]any{"type": "string", "context": "example-mod.territory"},
			},
		},
	})
	body["contexts"] = []map[string]any{
		{"id": "example-mod.territory", "name": "Territory", "namespace": "example-mod"},
		{"id": "example-mod.vacant", "name": "Vacant lots", "namespace": "example-mod"},
	}
	return body
}

// enumerationRead is one Admin API read of a context's entries, started in
// the background because the hub holds it until the plugin answers.
type enumerationRead struct {
	Status int
	Body   []byte
	Err    error
}

func (e Env) readEntriesInBackground(ctx context.Context, serverID, contextID string) <-chan enumerationRead {
	done := make(chan enumerationRead, 1)
	go func() {
		response, body, err := e.do(ctx, http.MethodGet,
			"/api/v1/servers/"+serverID+"/contexts/"+contextID+"/entries", e.AdminToken, nil)
		read := enumerationRead{Err: err, Body: body}
		if response != nil {
			read.Status = response.StatusCode
		}
		done <- read
	}()
	return done
}

// awaitEnumerate polls, acking as it goes, until a context.enumerate for the
// wanted context arrives, and returns its body.
func (p *fakePlugin) awaitEnumerate(ctx context.Context, want string, attempts int) (requestID string, err error) {
	for range attempts {
		response, err := p.pollAndAck(ctx)
		if err != nil {
			return "", err
		}
		for _, delivered := range response.Envelopes {
			if delivered.Type != "context.enumerate" {
				continue
			}
			var body struct {
				RequestID string `json:"requestId"`
				Context   string `json:"context"`
			}
			if err := json.Unmarshal(delivered.Body, &body); err != nil {
				return "", fmt.Errorf("decode context.enumerate body %q: %w", truncate(delivered.Body), err)
			}
			if body.RequestID == "" || utf8.RuneCountInString(body.RequestID) > 128 {
				return "", fmt.Errorf("context.enumerate carries requestId %q; it is hub-assigned, non-empty, and at most 128 code points (section 6.2)", body.RequestID)
			}
			if body.Context != want {
				return "", fmt.Errorf("context.enumerate names context %q, want the %q that was read (section 6.2)", body.Context, want)
			}
			return body.RequestID, nil
		}
	}
	return "", fmt.Errorf("no context.enumerate for %q reached the plugin in %d polls; a hub that exposes the Admin API read asks the plugin (section 6.2)", want, attempts)
}

// gradeEnumerationRead decodes and checks a 200 answer.
func gradeEnumerationRead(read enumerationRead, wantContext string, wantEntries int) (contextEntriesRecord, error) {
	var record contextEntriesRecord
	if read.Err != nil {
		return record, read.Err
	}
	if read.Status != http.StatusOK {
		return record, fmt.Errorf("GET .../contexts/%s/entries answered %d %s, want 200 with the plugin's entries", wantContext, read.Status, truncate(read.Body))
	}
	if err := json.Unmarshal(read.Body, &record); err != nil {
		return record, fmt.Errorf("decode the enumeration %q: %w", truncate(read.Body), err)
	}
	if record.Context != wantContext {
		return record, fmt.Errorf("the enumeration names context %q, want %q", record.Context, wantContext)
	}
	if record.Entries == nil {
		return record, fmt.Errorf("the enumeration carries no entries array; it is always present, empty when the plugin offered nothing")
	}
	if len(record.Entries) != wantEntries {
		return record, fmt.Errorf("the enumeration carries %d entries, want the plugin's %d verbatim and whole", len(record.Entries), wantEntries)
	}
	if _, err := time.Parse(time.RFC3339, record.EnumeratedAt); err != nil {
		return record, fmt.Errorf("enumeratedAt %q is not RFC 3339", record.EnumeratedAt)
	}
	return record, nil
}

var contextChecks = []Check{
	{
		ID:      "admin.contexts.enumerate",
		Title:   "A context read asks the plugin once and hands its entries back verbatim",
		Section: "6.2",
		Run: func(ctx context.Context, env Env) error {
			plugin, err := env.newFakePlugin(ctx, "conformance: context enumerate", shortPollTimeoutSeconds)
			if err != nil {
				return err
			}
			serverID := plugin.Server.Server.ID
			if _, err := plugin.publishManifest(ctx, contextManifest(1)); err != nil {
				return err
			}

			// The read holds; the question reaches the plugin on its poll.
			pending := env.readEntriesInBackground(ctx, serverID, "example-mod.territory")
			requestID, err := plugin.awaitEnumerate(ctx, "example-mod.territory", 3)
			if err != nil {
				return err
			}

			// A reply echoing a requestId nobody asked with is acked and
			// ignored; the read keeps waiting.
			stray := plugin.nextOutbound("context.entries", map[string]any{
				"requestId": "conformance-never-asked-" + strconv.FormatInt(time.Now().UnixNano(), 36), "context": "example-mod.territory",
				"entries": []map[string]any{{"referenceKey": "stray", "label": "Stray"}},
			})
			response, err := plugin.send(ctx, stray)
			if err != nil {
				return err
			}
			if response.Ack < stray.Seq {
				return fmt.Errorf("ack = %d after an unsolicited context.entries, want %d: a reply nobody asked for is acked and ignored (section 6.2)", response.Ack, stray.Seq)
			}
			select {
			case read := <-pending:
				return fmt.Errorf("the read answered %d on a reply that did not echo its requestId; the echo is how the answer is matched to the question (section 6.2)", read.Status)
			case <-time.After(300 * time.Millisecond):
			}

			entries := []map[string]any{
				{"referenceKey": "north-ridge", "label": "North Ridge", "position": []float64{4231.5, 300.2, 10620}},
				{"referenceKey": "south-bay", "label": "South Bay", "data": map[string]any{"owner": "clan-a"}},
			}
			if _, err := plugin.send(ctx, plugin.nextOutbound("context.entries", map[string]any{
				"requestId": requestID, "context": "example-mod.territory", "entries": entries,
			})); err != nil {
				return err
			}
			var read enumerationRead
			select {
			case read = <-pending:
			case <-ctx.Done():
				return ctx.Err()
			}
			record, err := gradeEnumerationRead(read, "example-mod.territory", 2)
			if err != nil {
				return err
			}
			if record.Reason != nil {
				return fmt.Errorf("reason = %q on a reply that carried none, want null", *record.Reason)
			}
			var second struct {
				ReferenceKey string          `json:"referenceKey"`
				Label        string          `json:"label"`
				Data         json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal(record.Entries[1], &second); err != nil || second.ReferenceKey != "south-bay" ||
				second.Label != "South Bay" || !strings.Contains(string(second.Data), "clan-a") {
				return fmt.Errorf("entries[1] = %s, want the plugin's entry verbatim, data included", truncate(record.Entries[1]))
			}

			// A reply with nothing to offer and a reason is surfaced as such.
			vacant := env.readEntriesInBackground(ctx, serverID, "example-mod.vacant")
			requestID, err = plugin.awaitEnumerate(ctx, "example-mod.vacant", 3)
			if err != nil {
				return err
			}
			if _, err := plugin.send(ctx, plugin.nextOutbound("context.entries", map[string]any{
				"requestId": requestID, "context": "example-mod.vacant", "entries": []any{},
				"reason": "nothing is vacant",
			})); err != nil {
				return err
			}
			select {
			case read = <-vacant:
			case <-ctx.Done():
				return ctx.Err()
			}
			record, err = gradeEnumerationRead(read, "example-mod.vacant", 0)
			if err != nil {
				return err
			}
			if record.Reason == nil || *record.Reason != "nothing is vacant" {
				return fmt.Errorf("reason = %v, want the plugin's \"nothing is vacant\" surfaced", record.Reason)
			}

			// What the read refuses without asking: a context the manifest does
			// not declare, a built-in context, an unknown server, no token.
			for _, path := range []string{
				"/api/v1/servers/" + serverID + "/contexts/example-mod.undeclared/entries",
				"/api/v1/servers/" + serverID + "/contexts/player/entries",
				"/api/v1/servers/conformance-no-such-server/contexts/example-mod.territory/entries",
			} {
				if err := env.expectError(ctx, http.MethodGet, path, env.AdminToken, nil, http.StatusNotFound, "not_found"); err != nil {
					return err
				}
			}
			if err := env.expectError(ctx, http.MethodGet,
				"/api/v1/servers/"+serverID+"/contexts/example-mod.territory/entries", "",
				nil, http.StatusUnauthorized, "unauthorized"); err != nil {
				return err
			}
			// Nothing above may have reached the plugin.
			quiet, err := plugin.pollAndAck(ctx)
			if err != nil {
				return err
			}
			for _, delivered := range quiet.Envelopes {
				if delivered.Type == "context.enumerate" {
					return fmt.Errorf("a refused read still sent a context.enumerate (%s); a context the manifest does not declare is refused without asking (section 6.2)", truncate(delivered.Body))
				}
			}
			return nil
		},
	},
	{
		ID:      "plugin.manifest.contextAnnotation",
		Title:   "A context annotation must name a declared context on a string schema",
		Section: "6.1",
		Run: func(ctx context.Context, env Env) error {
			plugin, err := env.newFakePlugin(ctx, "conformance: context annotation", shortPollTimeoutSeconds)
			if err != nil {
				return err
			}
			serverID := plugin.Server.Server.ID

			// Declared: accepted and stored verbatim, annotation included.
			if _, err := plugin.publishManifest(ctx, contextManifest(1)); err != nil {
				return err
			}
			record, err := env.storedManifest(ctx, serverID)
			if err != nil {
				return err
			}
			if record.Revision != 1 {
				return fmt.Errorf("revision = %d, want 1: a manifest whose annotation names a declared context is accepted (section 6.1)", record.Revision)
			}
			if !strings.Contains(string(record.Manifest), `"context":"example-mod.territory"`) &&
				!strings.Contains(string(record.Manifest), `"context": "example-mod.territory"`) {
				return fmt.Errorf("the stored manifest does not carry the annotation verbatim: %s", truncate(record.Manifest))
			}

			// The annotation never constrains a dispatch.
			if _, _, err := env.dispatchAction(ctx, serverID, map[string]any{
				"code": "example-mod.claim", "context": "example-mod.territory", "referenceKey": "anything",
				"params": map[string]any{"neighbour": "never-enumerated"},
			}); err != nil {
				return fmt.Errorf("a dispatch with a value outside any enumeration was refused; the annotation is advisory (section 6.1): %w", err)
			}

			// Undeclared, and on a non-string schema: each rejected at the
			// annotation's path, the stored manifest untouched.
			undeclared := contextManifest(2)
			undeclared["contexts"] = []any{}
			nonString := contextManifest(3)
			nonString["actions"].([]map[string]any)[0]["params"] = map[string]any{
				"type":       "object",
				"properties": map[string]any{"count": map[string]any{"type": "integer", "context": "example-mod.territory"}},
			}
			for _, invalid := range []map[string]any{undeclared, nonString} {
				published := plugin.nextOutbound("manifest.publish", invalid)
				response, err := plugin.pollAndAck(ctx, published)
				if err != nil {
					return err
				}
				if response.Ack < published.Seq {
					return fmt.Errorf("ack = %d, want %d: a rejected manifest is still acked (section 6.4)", response.Ack, published.Seq)
				}
				var rejects []envelope
				for _, delivered := range response.Envelopes {
					if delivered.Type == "manifest.reject" {
						rejects = append(rejects, delivered)
					}
				}
				if len(rejects) == 0 {
					if rejects, err = plugin.awaitEnvelope(ctx, "manifest.reject", 3); err != nil {
						return fmt.Errorf("%w; a context annotation naming an undeclared context, or sitting on a non-string schema, rejects the manifest (section 6.4)", err)
					}
				}
				var reject struct {
					EnvelopeID string `json:"envelopeId"`
					Errors     []struct {
						Path string `json:"path"`
					} `json:"errors"`
				}
				if err := json.Unmarshal(rejects[0].Body, &reject); err != nil {
					return fmt.Errorf("decode manifest.reject body %q: %w", truncate(rejects[0].Body), err)
				}
				if reject.EnvelopeID != published.ID {
					return fmt.Errorf("manifest.reject names envelope %q, want the rejected %q", reject.EnvelopeID, published.ID)
				}
				named := false
				for _, fault := range reject.Errors {
					if strings.HasSuffix(fault.Path, ".context") {
						named = true
					}
				}
				if !named {
					return fmt.Errorf("manifest.reject %s names no fault at a .context path; the annotation is what was wrong", truncate(rejects[0].Body))
				}
			}
			record, err = env.storedManifest(ctx, serverID)
			if err != nil {
				return err
			}
			if record.Revision != 1 {
				return fmt.Errorf("revision = %d after the rejected publishes, want the accepted 1 untouched", record.Revision)
			}
			return nil
		},
	},
}

func init() {
	checks = append(checks, contextChecks...)
}
