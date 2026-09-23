package hub

import (
	"strings"
	"testing"
)

// Redaction must strip every copy of a member, not only the one a decoder
// keeps: JSON lets an object repeat a member name, a receiver's parser may
// keep either copy, and an escaped spelling is the same name (spec section
// 11.2).
func TestRedactionStripsEveryCopy(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, data, path, leaked string
	}{
		{"a duplicate parent keeps the stripped member in its first copy",
			`{"killer":{"position":[123,456]},"killer":{}}`, "killer.position", "123"},
		{"a duplicate grandparent",
			`{"a":{"killer":{"position":[123]}},"a":{}}`, "a.killer.position", "123"},
		{"an escaped spelling of the parent",
			`{"kill\u0065r":{"position":[123]},"killer":{}}`, "killer.position", "123"},
		{"a duplicate leaf",
			`{"position":[123],"position":[456]}`, "position", "position"},
		{"a duplicate inside an array element",
			`{"crew":[{"position":[123],"position":[456]}]}`, "crew.position", "position"},
		{"an escaped leaf",
			`{"positio\u006e":[123]}`, "position", "123"},
	} {
		redacted := string(redactData([]byte(tc.data), []string{tc.path}))
		if strings.Contains(redacted, tc.leaked) {
			t.Errorf("%s: %s redacted by %q is %s, which still carries %q", tc.name, tc.data, tc.path, redacted, tc.leaked)
		}
	}

	// A path that reaches nothing leaves the plugin's bytes alone.
	untouched := `{"x": 12345678901234567890, "y": "<b>"}`
	if got := string(redactData([]byte(untouched), []string{"z", "q.deeper"})); got != untouched {
		t.Errorf("unreached data became %s, want the bytes as sent", got)
	}
}
