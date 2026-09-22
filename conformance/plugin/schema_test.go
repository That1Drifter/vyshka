package main

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"
)

func TestSynthesizeStaysInsideTheSchemaWhenStepping(t *testing.T) {
	many := []any{"conformance"}
	for i := 1; i <= 150; i++ {
		many = append(many, "conformance-"+strconv.Itoa(i))
	}
	schema := map[string]any{
		"type":     "object",
		"required": []any{"low", "name", "parts"},
		"properties": map[string]any{
			// The first candidate is 0; stepping up would break maximum.
			"low":  map[string]any{"type": "integer", "maximum": float64(0), "not": map[string]any{"enum": []any{float64(0)}}},
			"name": map[string]any{"type": "string", "not": map[string]any{"enum": many}},
			// A default whose item the items schema excludes is no default.
			"parts": map[string]any{
				"type":    "array",
				"default": []any{"blocked"},
				"items":   map[string]any{"type": "string", "not": map[string]any{"enum": []any{"blocked"}}},
			},
		},
	}
	params := synthesizeParams(schema)
	encoded, _ := json.Marshal(params)
	var decoded any
	_ = json.Unmarshal(encoded, &decoded)
	if !satisfies(schema, decoded) {
		t.Fatalf("synthesized %s, which the schema refuses", encoded)
	}
}

func TestSynthesizeSearchesThePermittedDomain(t *testing.T) {
	blocked := map[string]any{"enum": []any{"blocked"}}
	for name, schema := range map[string]map[string]any{
		"fractionalRange": {"type": "number", "minimum": float64(0), "maximum": 0.5, "not": map[string]any{"enum": []any{float64(0)}}},
		"exclusiveStart":  {"type": "number", "exclusiveMinimum": float64(0), "maximum": 0.5, "not": map[string]any{"enum": []any{float64(0)}}},
		"objectDefault": {"type": "object", "default": map[string]any{"x": "blocked"},
			"properties": map[string]any{"x": map[string]any{"type": "string", "not": blocked}}},
		"compoundEnum": {"enum": []any{map[string]any{"x": "blocked"}, map[string]any{"x": "allowed"}},
			"properties": map[string]any{"x": map[string]any{"type": "string", "not": blocked}}},
		"emptyArrayExcluded": {"type": "array", "items": map[string]any{"type": "string"}, "not": map[string]any{"enum": []any{[]any{}}}},
		// Round three's cases: the sample points excluded, the first
		// fallback excluded, and keywords that apply without a type.
		"samplesExcluded": {"type": "number", "minimum": float64(0), "maximum": float64(1),
			"not": map[string]any{"enum": []any{float64(0), float64(1), 0.5, 0.25, 0.75, 0.125, 0.875}}},
		"arrayFallbackExcluded": {"type": "array", "items": map[string]any{"enum": []any{"x"}},
			"not": map[string]any{"enum": []any{[]any{}, []any{"x"}}}},
		"objectExcluded": {"type": "object", "not": map[string]any{"enum": []any{map[string]any{}}}},
		"untypedItems": {"enum": []any{[]any{"blocked"}, []any{"allowed"}},
			"items": map[string]any{"type": "string", "not": blocked}},
		"untypedBound":    {"enum": []any{float64(0), float64(2)}, "minimum": float64(1)},
		"untypedRequired": {"enum": []any{map[string]any{}, map[string]any{"x": float64(1)}}, "required": []any{"x"}},
		// Round four's cases: a generated key the schema declares, an
		// exclusion list past the finest sampled point, and an untyped
		// schema whose object branch is impossible.
		"declaredGeneratedKey": {"type": "object", "properties": map[string]any{"conformance-1": map[string]any{"type": "string"}},
			"not": map[string]any{"enum": []any{map[string]any{}}}},
		"denseExclusion": {"type": "number", "minimum": float64(0), "maximum": float64(1), "not": map[string]any{"enum": everyFraction(4096)}},
		"impossibleObject": {"type": "object", "required": []any{"v"}, "properties": map[string]any{
			"v": map[string]any{"required": []any{"x"}, "properties": map[string]any{
				"x": map[string]any{"enum": []any{nil}, "not": map[string]any{"enum": []any{nil}}}}}}},
		// Round five's cases: required names that collide with the
		// generated ones, and a fallback list the schema excludes.
		"requiredCollide": {"type": "object", "required": []any{"conformance-1", "conformance-2"},
			"not": map[string]any{"enum": []any{map[string]any{"conformance-1": map[string]any{}, "conformance-2": map[string]any{}}}}},
		"fallbacksExcluded": {"type": "object", "required": []any{"v"}, "properties": map[string]any{
			"v": map[string]any{"required": []any{"x"},
				"properties": map[string]any{"x": map[string]any{"type": "null", "not": map[string]any{"enum": []any{nil}}}},
				"not":        map[string]any{"enum": []any{float64(1), "conformance", true, nil, []any{}}}}}},
	} {
		value := synthesizeValue(schema)
		encoded, _ := json.Marshal(value)
		var decoded any
		_ = json.Unmarshal(encoded, &decoded)
		if !satisfies(schema, decoded) {
			t.Errorf("%s: synthesized %s, which the schema refuses", name, encoded)
		}
	}
}

// everyFraction lists k/denominator for every k from 0 to denominator.
func everyFraction(denominator int) []any {
	members := make([]any, 0, denominator+1)
	for k := 0; k <= denominator; k++ {
		members = append(members, float64(k)/float64(denominator))
	}
	return members
}

// Keywords apply without a declared type, as a hub applies them, so these
// are checked against the value a hub accepts rather than against the
// suite's own validator.
func TestSynthesizeAppliesKeywordsWithoutAType(t *testing.T) {
	blocked := map[string]any{"enum": []any{"blocked"}}
	for name, tc := range map[string]struct {
		schema map[string]any
		want   string
	}{
		"items":    {map[string]any{"enum": []any{[]any{"blocked"}, []any{"allowed"}}, "items": map[string]any{"type": "string", "not": blocked}}, `["allowed"]`},
		"bound":    {map[string]any{"enum": []any{float64(0), float64(2)}, "minimum": float64(1)}, `2`},
		"required": {map[string]any{"enum": []any{map[string]any{}, map[string]any{"x": float64(1)}}, "required": []any{"x"}}, `{"x":1}`},
		"default":  {map[string]any{"default": []any{"blocked"}, "items": map[string]any{"not": blocked}, "enum": []any{[]any{"blocked"}, []any{"ok"}}}, `["ok"]`},
	} {
		encoded, _ := json.Marshal(synthesizeValue(tc.schema))
		if string(encoded) != tc.want {
			t.Errorf("%s: synthesized %s, want %s", name, encoded, tc.want)
		}
	}
}

// Round six's case: twenty nested arrays over an item nothing satisfies.
// Growing each array by re-synthesizing its item cost 3^20 calls; the
// untyped v still admits a number, which synthesis must reach promptly.
func TestSynthesizeNestedImpossibleArraysFinishPromptly(t *testing.T) {
	leaf := map[string]any{"type": "boolean", "not": map[string]any{"enum": []any{true, false}}}
	nested := leaf
	for i := 0; i < 20; i++ {
		nested = map[string]any{"type": "array", "items": nested, "not": map[string]any{"enum": []any{[]any{}}}}
	}
	schema := map[string]any{"type": "object", "required": []any{"v"}, "properties": map[string]any{
		"v": map[string]any{"required": []any{"x"}, "properties": map[string]any{"x": nested}}}}
	done := make(chan map[string]any, 1)
	go func() { done <- synthesizeParams(schema) }()
	select {
	case params := <-done:
		encoded, _ := json.Marshal(params)
		if string(encoded) != `{"v":1}` {
			t.Fatalf("synthesized %s, want {\"v\":1}", encoded)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("synthesis did not finish within 5 s")
	}
}

// Round seven's case: a required name listed twice at each of thirty levels
// synthesized the same subtree twice per level, 2^30 leaves in all.
func TestSynthesizeDuplicateRequiredNamesFinishPromptly(t *testing.T) {
	schema := map[string]any{"type": "string"}
	for i := 0; i < 30; i++ {
		schema = map[string]any{"type": "object", "required": []any{"x", "x"}, "properties": map[string]any{"x": schema}}
	}
	done := make(chan map[string]any, 1)
	go func() { done <- synthesizeParams(schema) }()
	select {
	case params := <-done:
		if !satisfies(schema, params) {
			encoded, _ := json.Marshal(params)
			t.Fatalf("synthesized %s, which the schema refuses", encoded)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("synthesis did not finish within 5 s")
	}
}

// Round eight's cases 1 and 5: params are an object even when the root's
// default or first enum member is not, and the invalid probe omits a
// required name however often it is listed.
func TestSynthesizeParamsAreAnObjectTheSchemaAdmits(t *testing.T) {
	for name, schema := range map[string]map[string]any{
		"scalarDefault": {"default": float64(1), "required": []any{"x"}},
		"scalarEnum":    {"enum": []any{float64(1), map[string]any{"x": float64(0)}}, "required": []any{"x"}},
	} {
		params := synthesizeParams(schema)
		encoded, _ := json.Marshal(params)
		if string(encoded) != `{"x":0}` && !(name == "scalarDefault" && params["x"] != nil) {
			t.Errorf("%s: synthesized %s, want an object carrying x", name, encoded)
		}
	}
	raw, _ := synthesizeInvalidParams(map[string]any{"type": "object", "required": []any{"x", "x", "y"},
		"properties": map[string]any{"x": map[string]any{"type": "string"}}})
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	if _, present := out["x"]; present {
		t.Errorf("invalid params %s still carry the required x the probe omits", raw)
	}
}

func TestValidateSubsetRejectsInexactConstants(t *testing.T) {
	for name, schema := range map[string]map[string]any{
		"notMember":  {"type": "number", "not": map[string]any{"enum": []any{float64(9007199254740993)}}},
		"nestedNot":  {"not": map[string]any{"enum": []any{map[string]any{"id": float64(9007199254740993)}}}},
		"enumMember": {"enum": []any{[]any{float64(-9007199254740993)}}},
		"bound":      {"type": "integer", "maximum": float64(9007199254740993)},
	} {
		if err := validateSubset(schema, "params", declaredNames{}); err == nil {
			t.Errorf("%s: a hub rejects a constant beyond 2^53, the suite accepted it", name)
		}
	}
}

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
	declared := declaredNames{contexts: map[string]bool{"driver.zone": true, "driver.item": true}}
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

func TestValidateSubsetKVNamespaceAnnotation(t *testing.T) {
	declared := declaredNames{kvNamespaces: map[string]bool{"driver.presets": true}}
	accepted := []map[string]any{
		{"type": "object", "properties": map[string]any{
			"preset": map[string]any{"type": "string", "kvNamespace": "driver.presets"},
		}},
		{"type": "string", "kvNamespace": nil},
	}
	for i, schema := range accepted {
		if err := validateSubset(schema, "params", declared); err != nil {
			t.Errorf("accepted[%d]: %v", i, err)
		}
	}
	rejected := map[string]map[string]any{
		"undeclared": {"type": "string", "kvNamespace": "driver.nothing"},
		"nonString":  {"type": "integer", "kvNamespace": "driver.presets"},
		"untyped":    {"kvNamespace": "driver.presets"},
		"array":      {"type": "string", "kvNamespace": []any{"driver.presets"}},
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
		if err := validateSubset(schema, "params", declaredNames{}); err != nil {
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
		if err := validateSubset(schema, "params", declaredNames{}); err == nil {
			t.Errorf("%s: a hub rejects this not, the suite accepted it", name)
		}
	}
}
