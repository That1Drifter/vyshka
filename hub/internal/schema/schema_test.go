package schema

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustCompile(t *testing.T, raw string) *Schema {
	t.Helper()
	compiled, faults := Compile(json.RawMessage(raw))
	if len(faults) > 0 {
		t.Fatalf("Compile(%s) faults: %v", raw, faults)
	}
	return compiled
}

func TestCompileAcceptsTheSubset(t *testing.T) {
	mustCompile(t, `{
		"type": "object",
		"required": ["amount"],
		"properties": {
			"amount":  { "type": "integer", "minimum": 1, "maximum": 100, "default": 50 },
			"ratio":   { "type": "number", "exclusiveMinimum": 0, "exclusiveMaximum": 1 },
			"targets": { "type": "array", "items": { "type": "string" } },
			"item":    { "type": "string", "x-vyshka-widget": "itemlist" },
			"mode":    { "enum": ["quick", "full"] },
			"anything": {}
		}
	}`)
}

func TestCompileRejectsKeywordsOutsideTheSubset(t *testing.T) {
	cases := []struct {
		name   string
		raw    string
		wantIn string
	}{
		{"pattern", `{"type": "string", "pattern": "^a"}`, "pattern"},
		{"additionalProperties", `{"type": "object", "additionalProperties": false}`, "additionalProperties"},
		{"ref", `{"$ref": "#/defs/x"}`, "$ref"},
		{"nested", `{"type": "object", "properties": {"a": {"minLength": 3}}}`, "properties.a"},
		{"insideItems", `{"type": "array", "items": {"format": "date-time"}}`, "items"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, faults := Compile(json.RawMessage(tc.raw))
			if len(faults) == 0 {
				t.Fatalf("Compile(%s) accepted an unsupported keyword", tc.raw)
			}
			if !strings.Contains(faults[0].Path, tc.wantIn) && !strings.Contains(faults[0].Message, tc.wantIn) {
				t.Fatalf("fault %q does not name %q", faults[0], tc.wantIn)
			}
		})
	}
}

func TestCompileRejectsMalformedSchemas(t *testing.T) {
	for name, raw := range map[string]string{
		"notJSON":        `{`,
		"notAnObject":    `true`,
		"badType":        `{"type": "text"}`,
		"typeArray":      `{"type": ["string", "null"]}`,
		"emptyEnum":      `{"enum": []}`,
		"requiredNumber": `{"required": [1]}`,
		"boundString":    `{"minimum": "one"}`,
		"widgetObject":   `{"x-vyshka-widget": {"kind": "itemlist"}}`,
		"itemsBoolean":   `{"items": true}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, faults := Compile(json.RawMessage(raw)); len(faults) == 0 {
				t.Fatalf("Compile(%s) accepted a malformed schema", raw)
			}
		})
	}
}

func TestCompileReportsEveryFault(t *testing.T) {
	_, faults := Compile(json.RawMessage(`{
		"type": "object",
		"properties": {
			"a": { "pattern": "^a" },
			"b": { "minLength": 3 }
		}
	}`))
	if len(faults) != 2 {
		t.Fatalf("want 2 faults, one per unsupported keyword, got %v", faults)
	}
}

func TestCompileBoundsRecursion(t *testing.T) {
	deep := strings.Repeat(`{"type":"array","items":`, maxDepth+2) + `{}` +
		strings.Repeat(`}`, maxDepth+2)
	if _, faults := Compile(json.RawMessage(deep)); len(faults) == 0 {
		t.Fatal("a schema nested past the depth cap compiled")
	}
}

func TestValidate(t *testing.T) {
	compiled := mustCompile(t, `{
		"type": "object",
		"required": ["amount"],
		"properties": {
			"amount":  { "type": "integer", "minimum": 1, "maximum": 100 },
			"targets": { "type": "array", "items": { "type": "string" } },
			"mode":    { "enum": ["quick", "full"] }
		}
	}`)

	valid := []string{
		`{"amount": 1}`,
		`{"amount": 100, "targets": ["a", "b"], "mode": "quick"}`,
		`{"amount": 50, "unknownField": true}`, // no additionalProperties in the subset
	}
	for _, instance := range valid {
		if faults := compiled.Validate(json.RawMessage(instance)); len(faults) > 0 {
			t.Errorf("Validate(%s) = %v, want no faults", instance, faults)
		}
	}

	invalid := map[string]string{
		`{}`:                            "amount",  // required
		`{"amount": 0}`:                 "minimum", // below the bound
		`{"amount": 101}`:               "maximum", // above the bound
		`{"amount": 2.5}`:               "integer", // not integral
		`{"amount": "1"}`:               "integer", // wrong type
		`{"amount": 1, "mode": 3}`:      "enum",    // not a member
		`{"amount": 1, "targets": [1]}`: "string",  // element type
		`[]`:                            "object",  // root type
	}
	for instance, wantIn := range invalid {
		faults := compiled.Validate(json.RawMessage(instance))
		if len(faults) == 0 {
			t.Errorf("Validate(%s) found nothing, want a fault about %q", instance, wantIn)
			continue
		}
		if !strings.Contains(faults[0].Path+" "+faults[0].Message, wantIn) {
			t.Errorf("Validate(%s) fault %q does not mention %q", instance, faults[0], wantIn)
		}
	}
}

func TestValidateExclusiveBounds(t *testing.T) {
	compiled := mustCompile(t, `{"type": "number", "exclusiveMinimum": 0, "exclusiveMaximum": 1}`)
	if faults := compiled.Validate(json.RawMessage(`0.5`)); len(faults) > 0 {
		t.Fatalf("0.5 in (0,1) failed: %v", faults)
	}
	for _, edge := range []string{`0`, `1`} {
		if faults := compiled.Validate(json.RawMessage(edge)); len(faults) == 0 {
			t.Fatalf("%s passed exclusive bounds (0,1)", edge)
		}
	}
}

func TestValidateEmptySchemaAcceptsAnything(t *testing.T) {
	compiled := mustCompile(t, `{}`)
	for _, instance := range []string{`null`, `42`, `"x"`, `[1,2]`, `{"a":1}`} {
		if faults := compiled.Validate(json.RawMessage(instance)); len(faults) > 0 {
			t.Errorf("empty schema rejected %s: %v", instance, faults)
		}
	}
}

func TestValidateEnumDeepEquality(t *testing.T) {
	compiled := mustCompile(t, `{"enum": [{"x": 1}, [1, 2], "plain"]}`)
	for _, member := range []string{`{"x": 1}`, `[1, 2]`, `"plain"`} {
		if faults := compiled.Validate(json.RawMessage(member)); len(faults) > 0 {
			t.Errorf("enum member %s rejected: %v", member, faults)
		}
	}
	if faults := compiled.Validate(json.RawMessage(`{"x": 2}`)); len(faults) == 0 {
		t.Error("a non-member object passed the enum")
	}
}
