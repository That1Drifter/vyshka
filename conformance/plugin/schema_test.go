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

func TestValidateSubsetContextAnnotation(t *testing.T) {
	declared := map[string]bool{"driver.zone": true, "driver.item": true}
	accepted := []map[string]any{
		{"type": "object", "properties": map[string]any{
			"zone": map[string]any{"type": "string", "context": "driver.zone"},
		}},
		{"type": "array", "items": map[string]any{"type": "string", "context": []any{"driver.zone", "driver.item"}}},
	}
	for i, schema := range accepted {
		if err := validateSubset(schema, "params", declared); err != nil {
			t.Errorf("accepted[%d]: %v", i, err)
		}
	}
	rejected := map[string]map[string]any{
		"undeclared": {"type": "string", "context": "driver.nothing"},
		"nonString":  {"type": "integer", "context": "driver.zone"},
		"untyped":    {"context": "driver.zone"},
		"empty":      {"type": "string", "context": []any{}},
		"number":     {"type": "string", "context": 3},
		"duplicate":  {"type": "string", "context": []any{"driver.zone", "driver.zone"}},
	}
	for name, schema := range rejected {
		if err := validateSubset(schema, "params", declared); err == nil {
			t.Errorf("%s: a hub rejects this annotation, the suite accepted it", name)
		}
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

func TestSynthesizeParamsStepsPastExcludedValues(t *testing.T) {
	schema := map[string]any{
		"type":     "object",
		"required": []any{"className", "count", "mode", "armed", "named"},
		"properties": map[string]any{
			"className": map[string]any{"type": "string", "not": map[string]any{"enum": []any{"conformance", "conformance-1"}}},
			"count":     map[string]any{"type": "integer", "minimum": float64(1), "not": map[string]any{"enum": []any{float64(1), float64(2)}}},
			"mode":      map[string]any{"enum": []any{"a", "b"}, "not": map[string]any{"enum": []any{"a"}}},
			"armed":     map[string]any{"type": "boolean", "not": map[string]any{"enum": []any{true}}},
			"named":     map[string]any{"type": "string", "default": "blocked", "not": map[string]any{"enum": []any{"blocked"}}},
		},
	}
	params := synthesizeParams(schema)
	want := map[string]any{"className": "conformance-2", "count": int64(3), "mode": "b", "armed": false, "named": "conformance"}
	for key, value := range want {
		if params[key] != value {
			t.Errorf("%s = %#v, want %#v: the synthesized value must avoid what not excludes", key, params[key], value)
		}
	}
}

func TestValidateSubsetExclusion(t *testing.T) {
	accepted := []map[string]any{
		{"type": "string", "not": map[string]any{"enum": []any{"M79"}}},
		{"type": "string", "not": nil},
	}
	for i, schema := range accepted {
		if err := validateSubset(schema, "params", nil); err != nil {
			t.Errorf("accepted[%d]: %v", i, err)
		}
	}
	rejected := map[string]map[string]any{
		"string":    {"type": "string", "not": "M79"},
		"typeForm":  {"type": "string", "not": map[string]any{"type": "string"}},
		"extraKey":  {"type": "string", "not": map[string]any{"enum": []any{"M79"}, "type": "string"}},
		"emptyEnum": {"type": "string", "not": map[string]any{"enum": []any{}}},
	}
	for name, schema := range rejected {
		if err := validateSubset(schema, "params", nil); err == nil {
			t.Errorf("%s: a hub rejects this not, the suite accepted it", name)
		}
	}
}
