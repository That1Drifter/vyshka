package params

import (
	"strings"
	"testing"
)

// A schema's root constraints are checked too, not only its properties'.
func TestRootConstraintsAreValidated(t *testing.T) {
	excluded := schemaOf(t, `{"type":"object","not":{"enum":[{}]},"properties":{"mode":{"type":"string"}}}`)
	if _, err := Coerce(excluded, nil); err == nil || !strings.Contains(err.Error(), "params: {} is excluded") {
		t.Errorf("an empty object excluded at the root passed: %v", err)
	}
	if _, err := Coerce(excluded, []string{"mode=soft"}); err != nil {
		t.Errorf("a non-empty object was refused: %v", err)
	}
	only := schemaOf(t, `{"type":"object","enum":[{"mode":"soft"}],"properties":{"mode":{"type":"string"}}}`)
	if _, err := Coerce(only, []string{"mode=hard"}); err == nil || !strings.Contains(err.Error(), "allowed values") {
		t.Errorf("a root enum was not enforced: %v", err)
	}
	if _, err := Coerce(only, []string{"mode=soft"}); err != nil {
		t.Errorf("the one allowed object was refused: %v", err)
	}
	// The messages for property faults keep naming the key alone.
	bounded := schemaOf(t, testSchema)
	if _, err := Coerce(bounded, []string{"amount=500"}); err == nil || err.Error() != "amount: 500 is above the maximum 100" {
		t.Errorf("property fault message = %v", err)
	}
	if _, err := Coerce(bounded, nil); err == nil || err.Error() != "missing required params: amount" {
		t.Errorf("missing required message = %v", err)
	}
}

// Lenient never fails on a value: what does not read as JSON within range
// is the text itself, and only a malformed argument is an error.
func TestLenientNeverFailsOnAValue(t *testing.T) {
	got, err := Lenient([]string{"x=1e400", "n=5", "s=hello", "b=true", "j:={\"a\":1}"})
	if err != nil {
		t.Fatalf("Lenient: %v", err)
	}
	if got["x"] != "1e400" || got["n"] != int64(5) || got["s"] != "hello" || got["b"] != true {
		t.Errorf("Lenient read %v", got)
	}
	if _, err := Lenient([]string{"x=1", "x=2"}); err == nil {
		t.Error("a key given twice passed")
	}
	if _, err := Lenient([]string{"j:={not json"}); err == nil {
		t.Error("raw JSON that does not parse passed")
	}
}

// Enum members compare as the hub compares them, as decoded JSON values, so
// a negative zero equals zero on both sides.
func TestNegativeZeroComparesAsZero(t *testing.T) {
	allowed := schemaOf(t, `{"type":"object","properties":{"x":{"type":"number","enum":[0]}}}`)
	if _, err := Coerce(allowed, []string{"x=-0"}); err != nil {
		t.Errorf("x=-0 against enum [0]: %v", err)
	}
	if _, err := Coerce(allowed, []string{"x:=-0.0"}); err != nil {
		t.Errorf("x:=-0.0 against enum [0]: %v", err)
	}
	excluded := schemaOf(t, `{"type":"object","properties":{"x":{"type":"number","not":{"enum":[0]}}}}`)
	if _, err := Coerce(excluded, []string{"x=-0"}); err == nil {
		t.Error("x=-0 passed not.enum [0]")
	}
	nested := schemaOf(t, `{"type":"object","properties":{"v":{"type":"array","not":{"enum":[[0,0]]}}}}`)
	if _, err := Coerce(nested, []string{"v:=[-0.0,0]"}); err == nil {
		t.Error("v:=[-0.0,0] passed not.enum [[0,0]]")
	}
}
