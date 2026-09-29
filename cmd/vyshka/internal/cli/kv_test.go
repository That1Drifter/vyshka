package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestKV(t *testing.T) {
	t.Parallel()
	h := newTestHub(t)

	steps := []struct {
		args   []string
		code   int
		stdout string // exact, when set
		stderr string // contained, when set
	}{
		{[]string{"kv", "set", "example-mod", "balance", "5"}, ExitOK, "revision 1\n", ""},
		{[]string{"kv", "get", "example-mod", "balance"}, ExitOK, "5\n", "revision 1"},
		{[]string{"kv", "set", "example-mod", "balance", `{"a": [1, 2]}`}, ExitOK, "revision 2\n", ""},
		{[]string{"kv", "get", "example-mod", "balance"}, ExitOK, `{"a":[1,2]}` + "\n", "revision 2"},
		{[]string{"kv", "set", "example-mod", "balance", "hello world"}, ExitOK, "revision 3\n", ""},
		{[]string{"kv", "get", "example-mod", "balance"}, ExitOK, `"hello world"` + "\n", ""},
		{[]string{"kv", "set", "example-mod", "balance", "5", "--string"}, ExitOK, "revision 4\n", ""},
		{[]string{"kv", "get", "example-mod", "balance"}, ExitOK, `"5"` + "\n", ""},
		{[]string{"kv", "set", "example-mod", "balance", "-5"}, ExitOK, "revision 5\n", ""},
		{[]string{"kv", "get", "example-mod", "balance"}, ExitOK, "-5\n", ""},
		// A compare-and-swap that loses says where the key really is.
		{[]string{"kv", "set", "example-mod", "balance", "6", "--if-revision", "1"}, ExitRefused, "", "current revision 5"},
		{[]string{"kv", "set", "example-mod", "balance", "6", "--if-revision", "5"}, ExitOK, "revision 6\n", ""},
		// Revision 0 means only if absent, and must reach the hub as 0.
		{[]string{"kv", "set", "example-mod", "balance", "7", "--if-revision", "0"}, ExitRefused, "", "revision_mismatch"},
		{[]string{"kv", "set", "example-mod", "fresh", "1", "--if-revision", "0", "--ttl", "1h"}, ExitOK, "", ""},
		{[]string{"kv", "get", "example-mod", "fresh"}, ExitOK, "1\n", "expires "},
		{[]string{"kv", "set", "example-mod", "balance", "null"}, ExitUsage, "", "cannot be null"},
		{[]string{"kv", "incr", "example-mod", "counter"}, ExitOK, "1\n", "revision 1"},
		{[]string{"kv", "incr", "example-mod", "counter", "--delta", "10"}, ExitOK, "11\n", "revision 2"},
		{[]string{"kv", "incr", "example-mod", "counter", "--delta", "-3"}, ExitOK, "8\n", ""},
		// incr refuses a key that does not hold an integer.
		{[]string{"kv", "set", "example-mod", "balance", "text"}, ExitOK, "revision 7\n", ""},
		{[]string{"kv", "incr", "example-mod", "balance"}, ExitRefused, "", "conflict"},
		{[]string{"kv", "delete", "example-mod", "balance"}, ExitOK, "deleted\n", ""},
		{[]string{"kv", "delete", "example-mod", "balance"}, ExitOK, "already absent\n", ""},
		{[]string{"kv", "get", "example-mod", "balance"}, ExitRefused, "", "not_found"},
		{[]string{"kv", "frob"}, ExitUsage, "", "unknown kv subcommand"},
		{[]string{"kv", "get", "example-mod"}, ExitUsage, "", "takes NS and KEY"},
	}
	for _, step := range steps {
		got := h.run(step.args...)
		if got.code != step.code {
			t.Errorf("%v: %v\nwant exit %d", step.args, got, step.code)
			continue
		}
		if step.stdout != "" && got.stdout != step.stdout {
			t.Errorf("%v: stdout %q, want %q", step.args, got.stdout, step.stdout)
		}
		if step.stderr != "" && !strings.Contains(got.stderr, step.stderr) {
			t.Errorf("%v: stderr %q lacks %q", step.args, got.stderr, step.stderr)
		}
	}

	// get's stdout is the value and nothing else, so it parses as JSON.
	h.run("kv", "set", "example-mod", "profile", `{"name":"Survivor","kills":3}`)
	got := h.run("kv", "get", "example-mod", "profile")
	var value map[string]any
	if err := json.Unmarshal([]byte(got.stdout), &value); err != nil || value["kills"] != float64(3) {
		t.Errorf("kv get stdout %q does not parse as the value: %v", got.stdout, err)
	}

	list := h.run("kv", "list", "example-mod")
	if list.code != ExitOK {
		t.Fatalf("kv list: %v", list)
	}
	for _, want := range []string{"KEY", "REVISION", "EXPIRES", "counter", "fresh", "profile", "never"} {
		if !strings.Contains(list.stdout, want) {
			t.Errorf("kv list lacks %q:\n%s", want, list.stdout)
		}
	}
	prefixed := h.run("kv", "list", "example-mod", "--prefix", "co")
	if prefixed.code != ExitOK || !strings.Contains(prefixed.stdout, "counter") || strings.Contains(prefixed.stdout, "profile") {
		t.Errorf("kv list --prefix: %v", prefixed)
	}
	paged := h.run("kv", "list", "example-mod", "--limit", "1")
	if paged.code != ExitOK || strings.Count(paged.stdout, "\n") != 2 || !strings.Contains(paged.stderr, "--all") {
		t.Errorf("kv list --limit 1: %v", paged)
	}
	walked := h.run("kv", "list", "example-mod", "--limit", "1", "--all")
	if walked.code != ExitOK || strings.Count(walked.stdout, "\n") != 4 {
		t.Errorf("kv list --limit 1 --all: %v", walked)
	}

	namespaces := h.run("kv", "namespaces")
	if namespaces.code != ExitOK || !strings.Contains(namespaces.stdout, "NAMESPACE") ||
		!strings.Contains(namespaces.stdout, "example-mod") || !strings.Contains(namespaces.stdout, "3") {
		t.Errorf("kv namespaces: %v", namespaces)
	}
	raw := h.run("kv", "namespaces", "--json")
	var decoded struct {
		Namespaces []struct {
			Namespace string `json:"namespace"`
			Keys      int    `json:"keys"`
		} `json:"namespaces"`
	}
	if raw.code != ExitOK || json.Unmarshal([]byte(raw.stdout), &decoded) != nil || len(decoded.Namespaces) != 1 ||
		decoded.Namespaces[0].Keys != 3 {
		t.Errorf("kv namespaces --json: %v", raw)
	}

	entry := h.run("kv", "get", "example-mod", "counter", "--json")
	var kv map[string]any
	if entry.code != ExitOK || json.Unmarshal([]byte(entry.stdout), &kv) != nil || kv["revision"] != float64(3) ||
		entry.stderr != "" {
		t.Errorf("kv get --json: %v", entry)
	}
}
