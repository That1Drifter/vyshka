package main

import (
	"context"
	"fmt"
	"strings"
)

// The `kvNamespace` annotation of section 6.1: a string param may name a
// key/value namespace the manifest declares, whose keys a UI offers as the
// field's values. Like `context` it is an annotation, never a constraint on a
// dispatch, and like `context` a manifest naming an undeclared namespace, or
// putting the annotation on a non-string schema, is rejected (section 6.4).

// kvAnnotationManifest declares two KV namespaces and an action whose
// `preset` param draws its values from one of them.
func kvAnnotationManifest(revision int64) map[string]any {
	body := manifestBody(revision, map[string]any{
		"code": "example-mod.preset.apply", "name": "Apply preset", "context": "world",
		"namespace": "example-mod", "danger": "none",
		"params": map[string]any{
			"type":     "object",
			"required": []string{"preset"},
			"properties": map[string]any{
				"preset": map[string]any{"type": "string", "kvNamespace": "example-mod.presets"},
			},
		},
	})
	body["kvNamespaces"] = []string{"example-mod", "example-mod.presets"}
	return body
}

var kvAnnotationChecks = []Check{
	{
		ID:      "plugin.manifest.kvNamespaceAnnotation",
		Title:   "A kvNamespace annotation must name a declared namespace on a string schema",
		Section: "6.1",
		Run: func(ctx context.Context, env Env) error {
			plugin, err := env.newFakePlugin(ctx, "conformance: kvNamespace annotation", shortPollTimeoutSeconds)
			if err != nil {
				return err
			}
			serverID := plugin.Server.Server.ID

			// Declared: accepted and stored verbatim, annotation included.
			if _, err := plugin.publishManifest(ctx, kvAnnotationManifest(1)); err != nil {
				return err
			}
			record, err := env.storedManifest(ctx, serverID)
			if err != nil {
				return err
			}
			if record.Revision != 1 {
				return fmt.Errorf("revision = %d, want 1: a manifest whose kvNamespace annotation names a declared namespace is accepted (section 6.1)", record.Revision)
			}
			stored := strings.ReplaceAll(string(record.Manifest), " ", "")
			if !strings.Contains(stored, `"kvNamespace":"example-mod.presets"`) {
				return fmt.Errorf("the stored manifest does not carry the annotation verbatim: %s", truncate(record.Manifest))
			}

			// The annotation never constrains a dispatch: no key of that name
			// exists in the store, and the dispatch is queued all the same.
			if _, _, err := env.dispatchAction(ctx, serverID, map[string]any{
				"code": "example-mod.preset.apply", "context": "world",
				"params": map[string]any{"preset": "no-such-key"},
			}); err != nil {
				return fmt.Errorf("a dispatch naming a key the store does not hold was refused; the annotation is advisory (section 6.1): %w", err)
			}

			// Undeclared (the namespace missing from kvNamespaces, and no
			// kvNamespaces at all), and on a non-string schema: each rejected
			// at the annotation's path, the stored manifest untouched.
			undeclared := kvAnnotationManifest(2)
			undeclared["kvNamespaces"] = []string{"example-mod"}
			absent := kvAnnotationManifest(3)
			delete(absent, "kvNamespaces")
			nonString := kvAnnotationManifest(4)
			nonString["actions"].([]map[string]any)[0]["params"] = map[string]any{
				"type":       "object",
				"properties": map[string]any{"count": map[string]any{"type": "integer", "kvNamespace": "example-mod.presets"}},
			}
			fixtures := []struct {
				manifest map[string]any
				wantPath string // where the rejection must point
			}{
				{undeclared, "actions[0].params.properties.preset.kvNamespace"},
				{absent, "actions[0].params.properties.preset.kvNamespace"},
				{nonString, "actions[0].params.properties.count.kvNamespace"},
			}
			for _, fixture := range fixtures {
				if err := plugin.expectManifestReject(ctx, fixture.manifest, fixture.wantPath,
					"a kvNamespace annotation naming an undeclared namespace, or sitting on a non-string schema, rejects the manifest (section 6.4)"); err != nil {
					return err
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
	checks = append(checks, kvAnnotationChecks...)
}
