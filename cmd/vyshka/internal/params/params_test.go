package params

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/That1Drifter/vyshka/client"
)

// schemaOf decodes a params schema from its manifest JSON, so the fixtures
// read like the manifests they stand for.
func schemaOf(t *testing.T, text string) *client.ParamsSchema {
	t.Helper()
	var schema client.ParamsSchema
	if err := json.Unmarshal([]byte(text), &schema); err != nil {
		t.Fatalf("schema %s: %v", text, err)
	}
	return &schema
}

const testSchema = `{
	"type": "object",
	"required": ["amount"],
	"properties": {
		"amount":      {"type": "integer", "minimum": 1, "maximum": 100},
		"ratio":       {"type": "number", "exclusiveMinimum": 0, "exclusiveMaximum": 1},
		"reason":      {"type": "string", "enum": ["cheating", "abuse"]},
		"item":        {"type": "string", "not": {"enum": ["Bomb"]}},
		"level":       {"type": "integer", "enum": [1, 5, 10]},
		"silent":      {"type": "boolean"},
		"nothing":     {"type": "null"},
		"position":    {"type": "array", "items": {"type": "number"}, "x-vyshka-widget": "vector"},
		"attachments": {"type": "array", "items": {"type": "string"}},
		"tags":        {"type": "array"},
		"counts":      {"type": "array", "items": {"type": "integer", "minimum": 0}},
		"loadout":     {"type": "object", "required": ["primary"],
		               "properties": {"primary": {"type": "string"}, "rounds": {"type": "integer", "maximum": 30}}},
		"anything":    {}
	}
}`

// wire is how the coerced params travel: what the hub will see.
func wire(t *testing.T, params map[string]any) string {
	t.Helper()
	encoded, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("encode %v: %v", params, err)
	}
	return string(encoded)
}

func TestCoerceTypesEachValueBySchema(t *testing.T) {
	schema := schemaOf(t, testSchema)
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"integer travels without a fraction", []string{"amount=5"}, `{"amount":5}`},
		{"number", []string{"amount=1", "ratio=0.25"}, `{"amount":1,"ratio":0.25}`},
		{"string verbatim, = inside the value", []string{"amount=1", "reason=cheating"}, `{"amount":1,"reason":"cheating"}`},
		{"a value may contain =", []string{"amount=1", "item=a=b"}, `{"amount":1,"item":"a=b"}`},
		{"boolean", []string{"amount=1", "silent=false"}, `{"amount":1,"silent":false}`},
		{"null", []string{"amount=1", "nothing=null"}, `{"amount":1,"nothing":null}`},
		{"vector", []string{"amount=1", "position=4501.2,320.1,9800.4"}, `{"amount":1,"position":[4501.2,320.1,9800.4]}`},
		{"two-component vector", []string{"amount=1", "position=1,2"}, `{"amount":1,"position":[1,2]}`},
		{"empty array", []string{"amount=1", "attachments="}, `{"amount":1,"attachments":[]}`},
		{"string array", []string{"amount=1", "attachments=scope,grip"}, `{"amount":1,"attachments":["scope","grip"]}`},
		{"untyped items are strings", []string{"amount=1", "tags=1,b"}, `{"amount":1,"tags":["1","b"]}`},
		{"integer items", []string{"amount=1", "counts=0,3"}, `{"amount":1,"counts":[0,3]}`},
		{"enum integer matches the schema's 5", []string{"amount=1", "level=5"}, `{"amount":1,"level":5}`},
		{"object through :=", []string{"amount=1", `loadout:={"primary":"M4A1","rounds":30}`},
			`{"amount":1,"loadout":{"primary":"M4A1","rounds":30}}`},
		{"untyped JSON", []string{"amount=1", "anything=[1,true]"}, `{"amount":1,"anything":[1,true]}`},
		{"untyped string", []string{"amount=1", "anything=hello world"}, `{"amount":1,"anything":"hello world"}`},
		{"raw integer", []string{"amount:=7"}, `{"amount":7}`},
		{"raw integral float is an integer", []string{"amount:=7.0"}, `{"amount":7}`},
	}
	for _, c := range cases {
		got, err := Coerce(schema, c.args)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if encoded := wire(t, got); encoded != c.want {
			t.Errorf("%s: %s, want %s", c.name, encoded, c.want)
		}
	}
}

func TestCoerceOutputTypes(t *testing.T) {
	got, err := Coerce(schemaOf(t, testSchema), []string{"amount=5", "ratio=0.5"})
	if err != nil {
		t.Fatalf("coerce: %v", err)
	}
	if _, ok := got["amount"].(int64); !ok {
		t.Errorf("amount is %T, want int64", got["amount"])
	}
	if _, ok := got["ratio"].(float64); !ok {
		t.Errorf("ratio is %T, want float64", got["ratio"])
	}
}

func TestCoerceRefusals(t *testing.T) {
	schema := schemaOf(t, testSchema)
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"above maximum", []string{"amount=500"}, "amount: 500 is above the maximum 100"},
		{"below minimum", []string{"amount=0"}, "amount: 0 is below the minimum 1"},
		{"exclusive minimum", []string{"amount=1", "ratio=0"}, "ratio: 0 is not above the exclusive minimum 0"},
		{"exclusive maximum", []string{"amount=1", "ratio=1"}, "ratio: 1 is not below the exclusive maximum 1"},
		{"not an integer", []string{"amount=5.5"}, `amount: "5.5" is not an integer`},
		{"not a number", []string{"amount=1", "ratio=lots"}, `ratio: "lots" is not a finite number`},
		{"NaN", []string{"amount=1", "ratio=NaN"}, `ratio: "NaN" is not a finite number`},
		{"Inf", []string{"amount=1", "ratio=Inf"}, `ratio: "Inf" is not a finite number`},
		{"hex float", []string{"amount=1", "ratio=0x1p-2"}, `ratio: "0x1p-2" is not a finite number`},
		{"boolean spelling", []string{"amount=1", "silent=yes"}, `silent: "yes" is not a boolean`},
		{"null spelling", []string{"amount=1", "nothing=nil"}, `nothing: "nil" is not null`},
		{"enum", []string{"amount=1", "reason=griefing"}, `reason: "griefing" is not one of the allowed values: "cheating", "abuse"`},
		{"enum integer", []string{"amount=1", "level=4"}, "level: 4 is not one of the allowed values: 1, 5, 10"},
		{"not.enum", []string{"amount=1", "item=Bomb"}, `item: "Bomb" is excluded by the schema`},
		{"not.enum is exact", []string{"amount=1", "item=bomb"}, ""},
		{"vector item", []string{"amount=1", "position=1,x,3"}, `position[1]: "x" is not a finite number`},
		{"item bound", []string{"amount=1", "counts=1,-1"}, "counts[1]: -1 is below the minimum 0"},
		{"object needs :=", []string{"amount=1", "loadout=M4A1"}, "loadout is an object, which key=value cannot express; pass it as loadout:=<json>"},
		{"raw type mismatch", []string{`amount:="5"`}, "amount: expected integer, got string"},
		{"raw nested required", []string{"amount=1", `loadout:={"rounds":3}`}, "loadout: missing required members: primary"},
		{"raw nested bound", []string{"amount=1", `loadout:={"primary":"x","rounds":31}`}, "loadout.rounds: 31 is above the maximum 30"},
		{"raw invalid JSON", []string{"amount:={"}, "amount: \"{\" is not valid JSON"},
		{"raw trailing data", []string{"amount:=1 2"}, "is not valid JSON"},
		{"unknown key", []string{"amt=5"}, `unknown param "amt"; declared params: amount, anything, attachments, counts, item, level, loadout, nothing, position, ratio, reason, silent, tags`},
		{"unknown raw key", []string{"amt:=5"}, `unknown param "amt"`},
		{"duplicate", []string{"amount=5", "amount=6"}, `param "amount" is given twice`},
		{"duplicate across forms", []string{"amount=5", "amount:=6"}, `param "amount" is given twice`},
		{"neither form", []string{"amount"}, `param "amount" is neither key=value nor key:=<json>`},
		{"no key", []string{"=5"}, `param "=5" has no key`},
		{"missing required", []string{"reason=abuse"}, "missing required params: amount"},
	}
	for _, c := range cases {
		_, err := Coerce(schema, c.args)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: unexpected refusal %v", c.name, err)
		case c.want == "":
		case err == nil:
			t.Errorf("%s: accepted, want %q", c.name, c.want)
		case !strings.Contains(err.Error(), c.want):
			t.Errorf("%s: error %q, want it to contain %q", c.name, err, c.want)
		case strings.Contains(err.Error(), "\n"):
			t.Errorf("%s: error spans lines: %q", c.name, err)
		}
	}
}

func TestMissingRequiredNamesEveryKeyAtOnce(t *testing.T) {
	schema := schemaOf(t, `{"type":"object","required":["amount","reason","target"],
		"properties":{"amount":{"type":"integer"},"reason":{"type":"string"},"target":{"type":"string"}}}`)
	_, err := Coerce(schema, []string{"reason=x"})
	if err == nil || err.Error() != "missing required params: amount, target" {
		t.Errorf("error = %v", err)
	}
}

func TestCoerceWithoutASchemaReadsJSONThenStrings(t *testing.T) {
	for _, schema := range []*client.ParamsSchema{nil, schemaOf(t, `{"type":"object"}`)} {
		got, err := Coerce(schema, []string{
			"count=5", "ratio=0.5", "flag=true", "name=Survivor", "list=[1,2]",
			"big=9007199254740993", `raw:={"a":1}`, "quoted=\"5\"", "empty=",
		})
		if err != nil {
			t.Fatalf("coerce: %v", err)
		}
		want := `{"big":9007199254740993,"count":5,"empty":"","flag":true,"list":[1,2],` +
			`"name":"Survivor","quoted":"5","ratio":0.5,"raw":{"a":1}}`
		if encoded := wire(t, got); encoded != want {
			t.Errorf("schema %v: %s, want %s", schema, encoded, want)
		}
	}

	// A value that is valid JSON but no float64 can hold is refused rather
	// than quietly sent as a string.
	if _, err := Coerce(nil, []string{"huge=1e400"}); err == nil || !strings.Contains(err.Error(), "out of range") {
		t.Errorf("1e400: error = %v, want out of range", err)
	}
}

func TestCoerceWithNoArgsAndNoRequirements(t *testing.T) {
	got, err := Coerce(schemaOf(t, `{"type":"object","properties":{"x":{"type":"string"}}}`), nil)
	if err != nil || len(got) != 0 {
		t.Errorf("got %v, %v; want an empty object", got, err)
	}
	if encoded := wire(t, got); encoded != "{}" {
		t.Errorf("wire = %s, want {}", encoded)
	}
}
