package main

import (
	"encoding/json"
	"testing"
)

func TestSynthesizeParamsSatisfiesTheDriverSchema(t *testing.T) {
	schema := map[string]any{
		"type":     "object",
		"required": []any{"amount"},
		"properties": map[string]any{
			"amount": map[string]any{"type": "integer", "minimum": float64(5), "maximum": float64(100)},
		},
	}
	params := synthesizeParams(schema)
	amount, ok := params["amount"].(int64)
	if !ok {
		t.Fatalf("amount is %T (%v), want an integer", params["amount"], params["amount"])
	}
	if amount < 5 || amount > 100 {
		t.Fatalf("amount %d is outside the schema's bounds", amount)
	}
}

func TestSynthesizeParamsPrefersDefaultsAndEnums(t *testing.T) {
	schema := map[string]any{
		"type":     "object",
		"required": []any{"mode", "level"},
		"properties": map[string]any{
			"mode":  map[string]any{"type": "string", "enum": []any{"gentle", "hard"}},
			"level": map[string]any{"type": "integer", "default": float64(7)},
		},
	}
	params := synthesizeParams(schema)
	if params["mode"] != "gentle" {
		t.Fatalf("mode is %v, want the first enum member", params["mode"])
	}
	if params["level"] != float64(7) {
		t.Fatalf("level is %v, want the declared default", params["level"])
	}
}

func TestSynthesizeInvalidParamsOmitsARequiredProperty(t *testing.T) {
	schema := map[string]any{
		"type":     "object",
		"required": []any{"amount"},
		"properties": map[string]any{
			"amount": map[string]any{"type": "integer"},
		},
	}
	raw, how := synthesizeInvalidParams(schema)
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("invalid params are not an object: %v", err)
	}
	if _, present := out["amount"]; present {
		t.Fatalf("invalid params %s still carry the required property (%s)", raw, how)
	}
}

func TestSynthesizeInvalidParamsWrongTypesAnOptionalProperty(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"note": map[string]any{"type": "string"},
		},
	}
	raw, _ := synthesizeInvalidParams(schema)
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("invalid params are not an object: %v", err)
	}
	if _, isString := out["note"].(string); isString || out["note"] == nil {
		t.Fatalf("invalid params %s do not violate the property's type", raw)
	}
}

func TestSynthesizeInvalidParamsFallsBackToANonObject(t *testing.T) {
	raw, how := synthesizeInvalidParams(nil)
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err == nil {
		t.Fatalf("an unconstrained schema produced an object (%s); nothing violates it, so params must not be an object", how)
	}
}
