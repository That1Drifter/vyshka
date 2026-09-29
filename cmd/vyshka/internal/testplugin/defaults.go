package testplugin

import "time"

// zoneContext is the one custom context the default manifest declares. An
// action drawing on it takes a zone's referenceKey on dispatch (spec section
// 6.2), so a client under test has a non-built-in context to exercise.
const zoneContext = "cli-test.zone"

// DefaultManifest is the manifest.publish body a plugin starts with when the
// caller supplies none. It is built fresh on every call, so a caller may edit
// the result (bump manifestRevision, drop an action) without touching another
// plugin's copy.
//
// The actions cover what a command-line client has to handle: a built-in
// context (player, world), a custom one, no context at all, every danger
// level, a schema with bounds, a required vector, an enum, and a schema that
// accepts anything. All constants sit well inside the hub's bounds (spec
// sections 6.1 and 6.4), and the schemas use only the section 6.1 subset, so a
// conforming hub accepts the whole manifest.
func DefaultManifest() map[string]any {
	object := func() map[string]any { return map[string]any{"type": "object"} }
	return map[string]any{
		"game":             defaultGame,
		"plugin":           map[string]any{"name": pluginName, "version": pluginVersion},
		"manifestRevision": 1,
		"actions": []map[string]any{
			{
				"code":      "cli-test.heal",
				"name":      "Heal",
				"context":   "player",
				"namespace": "cli-test",
				"danger":    "none",
				"params": map[string]any{
					"type":     "object",
					"required": []string{"amount"},
					"properties": map[string]any{
						"amount": map[string]any{"type": "integer", "minimum": 1, "maximum": 100},
					},
				},
			},
			{
				"code":      "cli-test.teleport",
				"name":      "Teleport",
				"context":   "player",
				"namespace": "cli-test",
				"danger":    "warning",
				"params": map[string]any{
					"type":     "object",
					"required": []string{"position"},
					"properties": map[string]any{
						"position": map[string]any{
							"type":            "array",
							"items":           map[string]any{"type": "number"},
							"x-vyshka-widget": "vector",
						},
					},
				},
			},
			{
				"code":      "cli-test.wipe",
				"name":      "Wipe",
				"context":   "world",
				"namespace": "cli-test",
				"danger":    "destructive",
				"params": map[string]any{
					"type":       "object",
					"properties": map[string]any{"reason": map[string]any{"type": "string"}},
				},
			},
			{
				"code":      "cli-test.echo",
				"name":      "Echo",
				"namespace": "cli-test",
				"danger":    "none",
				"params":    object(),
			},
			{
				"code":      "cli-test.slow",
				"name":      "Slow",
				"context":   "world",
				"namespace": "cli-test",
				"danger":    "none",
				"params": map[string]any{
					"type":     "object",
					"required": []string{"delayMs"},
					"properties": map[string]any{
						"delayMs": map[string]any{"type": "integer", "minimum": 0},
					},
				},
			},
			{
				"code":      "cli-test.fail",
				"name":      "Fail",
				"context":   "world",
				"namespace": "cli-test",
				"danger":    "none",
				"params":    object(),
			},
			{
				"code":      "cli-test.never",
				"name":      "Never",
				"context":   "world",
				"namespace": "cli-test",
				"danger":    "none",
				"params":    object(),
			},
			{
				"code":      "cli-test.pick",
				"name":      "Pick",
				"context":   zoneContext,
				"namespace": "cli-test",
				"danger":    "none",
				"params": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"mode": map[string]any{"type": "string", "enum": []string{"soft", "hard"}},
					},
				},
			},
		},
		"contexts": []map[string]any{
			{"id": zoneContext, "name": "Zones", "namespace": "cli-test"},
		},
		"events": []map[string]any{
			{"id": "cli-test.beat", "name": "Beat", "namespace": "cli-test"},
		},
		"kvNamespaces": []string{"cli-test"},
	}
}

// defaultContexts answers context.enumerate for the context DefaultManifest
// declares: one member with a position and one without, so a client sees both
// shapes of entry.
func defaultContexts() map[string][]ContextEntry {
	return map[string][]ContextEntry{
		zoneContext: {
			{ReferenceKey: "zone-a", Label: "Alpha", Position: []float64{100, 0, 200}},
			{ReferenceKey: "zone-b", Label: "Bravo"},
		},
	}
}

// DefaultHandler answers the actions DefaultManifest declares. Every dispatch
// is acked; what follows depends on the code, and an unknown code is answered
// with a failure rather than left to expire, so a client under test can tell a
// manifest that drifted from the handler apart from a lost result.
func DefaultHandler(d Dispatch) Outcome {
	succeed := func(result any) Outcome {
		return Outcome{Ack: true, Result: &Result{OK: true, Result: result}}
	}
	switch d.Code {
	case "cli-test.heal":
		return succeed(map[string]any{"healedTo": 100, "amount": d.Params["amount"]})
	case "cli-test.teleport":
		return succeed(map[string]any{"position": d.Params["position"]})
	case "cli-test.wipe":
		return succeed(map[string]any{"wiped": true})
	case "cli-test.echo":
		params := d.Params
		if params == nil {
			params = map[string]any{}
		}
		return succeed(map[string]any{"params": params})
	case "cli-test.slow":
		delayMs := int64(0)
		switch v := d.Params["delayMs"].(type) {
		case float64:
			delayMs = int64(v)
		case int:
			delayMs = int64(v)
		case int64:
			delayMs = v
		}
		delayMs = max(delayMs, 0)
		outcome := succeed(map[string]any{"slept": delayMs})
		outcome.Delay = time.Duration(delayMs) * time.Millisecond
		return outcome
	case "cli-test.fail":
		return Outcome{Ack: true, Result: &Result{OK: false, Error: "failed as asked"}}
	case "cli-test.never":
		// Acked, then silence: the hub expires the action at its deadline.
		return Outcome{Ack: true}
	case "cli-test.pick":
		return succeed(map[string]any{"referenceKey": d.ReferenceKey, "mode": d.Params["mode"]})
	}
	return Outcome{Ack: true, Result: &Result{OK: false, Error: "unknown action"}}
}
