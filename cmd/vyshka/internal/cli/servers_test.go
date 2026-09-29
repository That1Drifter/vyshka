package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestServersCreateListShow(t *testing.T) {
	t.Parallel()
	h := newTestHub(t)

	created := h.run("servers", "create", "Chernarus #1", "--game", "test-game", "--enrollment-ttl", "1h")
	if created.code != ExitOK {
		t.Fatalf("servers create: %v", created)
	}
	lines := strings.Split(strings.TrimSpace(created.stdout), "\n")
	if len(lines) != 4 || !strings.HasPrefix(lines[0], "server Chernarus #1 (") ||
		!strings.HasPrefix(lines[3], "expires ") {
		t.Fatalf("servers create output:\n%s", created.stdout)
	}
	// The token sits alone on its line, and it is a working one.
	token := lines[2]
	serverID := strings.TrimSuffix(strings.TrimPrefix(lines[0], "server Chernarus #1 ("), ")")
	h.enroll(serverID, token)

	var viaJSON struct {
		Server struct {
			ID string `json:"id"`
		} `json:"server"`
		Enrollment struct {
			Token string `json:"token"`
		} `json:"enrollment"`
	}
	got := h.run("--json", "servers", "create", "Livonia")
	if got.code != ExitOK || strings.Count(got.stdout, "\n") != 1 {
		t.Fatalf("servers create --json: %v", got)
	}
	if err := json.Unmarshal([]byte(got.stdout), &viaJSON); err != nil || viaJSON.Enrollment.Token == "" {
		t.Fatalf("servers create --json = %q (%v)", got.stdout, err)
	}

	list := h.run("servers")
	if list.code != ExitOK {
		t.Fatalf("servers: %v", list)
	}
	for _, want := range []string{"ID", "LINK", "CREDENTIALS", "PENDING", "LAST SEEN", "PLUGIN",
		"Chernarus #1", "Livonia", "cli-test-plugin 0.0.1", "never", "active", "none"} {
		if !strings.Contains(list.stdout, want) {
			t.Errorf("servers lacks %q:\n%s", want, list.stdout)
		}
	}

	show := h.run("servers", "show", serverID)
	if show.code != ExitOK {
		t.Fatalf("servers show: %v", show)
	}
	for _, want := range []string{"id:", serverID, "name:", "Chernarus #1", "credentials:", "active", "session:", "poll timeout 5s"} {
		if !strings.Contains(show.stdout, want) {
			t.Errorf("servers show lacks %q:\n%s", want, show.stdout)
		}
	}

	reissued := h.run("servers", "token", "livonia", "--ttl", "2h")
	if reissued.code != ExitOK {
		t.Fatalf("servers token: %v", reissued)
	}
	if lines := strings.Split(strings.TrimSpace(reissued.stdout), "\n"); len(lines) != 3 || lines[1] == viaJSON.Enrollment.Token {
		t.Errorf("servers token output:\n%s", reissued.stdout)
	}

	if got := h.run("servers", "create", "X", "--enrollment-ttl", "10ms"); got.code != ExitUsage {
		t.Errorf("a sub-second TTL was accepted: %v", got)
	}
	if got := h.run("servers", "frob"); got.code != ExitUsage {
		t.Errorf("servers frob: %v", got)
	}
}

func TestServerResolution(t *testing.T) {
	t.Parallel()
	h := newTestHub(t)
	chernarusID, _ := h.createServer("Chernarus #1")
	chernarus2ID, _ := h.createServer("Chernarus #2")
	livoniaID, _ := h.createServer("Livonia")
	h.createServer("livonia test")

	cases := []struct {
		arg, want string
	}{
		{chernarusID, chernarusID},     // the id itself
		{"chernarus #2", chernarus2ID}, // exact name, any case
		{"LIVONIA", livoniaID},         // an exact name beats a longer one containing it
		{"#1", chernarusID},            // a unique part of a name
	}
	for _, c := range cases {
		got := h.run("servers", "show", c.arg, "--json")
		var server struct {
			ID string `json:"id"`
		}
		if got.code != ExitOK || json.Unmarshal([]byte(got.stdout), &server) != nil || server.ID != c.want {
			t.Errorf("%q resolved to %q, want %q: %v", c.arg, server.ID, c.want, got)
		}
	}

	ambiguous := h.run("servers", "show", "chernarus")
	if ambiguous.code != ExitUsage || !strings.Contains(ambiguous.stderr, "more than one server") ||
		!strings.Contains(ambiguous.stderr, chernarusID) || !strings.Contains(ambiguous.stderr, chernarus2ID) {
		t.Errorf("ambiguous: %v", ambiguous)
	}
	none := h.run("servers", "show", "Takistan")
	if none.code != ExitUsage || !strings.Contains(none.stderr, "no server") || !strings.Contains(none.stderr, livoniaID) {
		t.Errorf("no match: %v", none)
	}
}

func TestActionsListing(t *testing.T) {
	t.Parallel()
	h := newTestHub(t)
	p := h.plugin("With manifest")
	p.publishManifest(testManifest(1))
	h.createServer("Bare")

	got := h.run("actions", "with manifest")
	if got.code != ExitOK {
		t.Fatalf("actions SERVER: %v", got)
	}
	for _, want := range []string{
		"CODE", "CONTEXT", "DANGER", "NAME", "PARAMS",
		"example-mod.heal", "warning", "amount:integer* reason:string",
		"position:number[]*", "example-mod.wipe", "destructive", "Wipe the map",
	} {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("actions lacks %q:\n%s", want, got.stdout)
		}
	}

	all := h.run("actions")
	if all.code != ExitOK || !strings.Contains(all.stdout, "Bare (") || !strings.Contains(all.stdout, "no manifest") ||
		!strings.Contains(all.stdout, "example-mod.teleport") {
		t.Errorf("actions for every server: %v", all)
	}

	detail := h.run("actions", "With manifest", "--code", "example-mod.heal")
	if detail.code != ExitOK {
		t.Fatalf("actions --code: %v", detail)
	}
	for _, want := range []string{"danger:", "warning", "params:", `"maximum": 100`, `"required": [`} {
		if !strings.Contains(detail.stdout, want) {
			t.Errorf("actions --code lacks %q:\n%s", want, detail.stdout)
		}
	}

	unknown := h.run("actions", "With manifest", "--code", "example-mod.nope")
	if unknown.code != ExitUsage || !strings.Contains(unknown.stderr, "example-mod.heal, example-mod.ping") {
		t.Errorf("actions --code unknown: %v", unknown)
	}
	if bare := h.run("actions", "Bare"); bare.code != ExitUsage || !strings.Contains(bare.stderr, "no manifest") {
		t.Errorf("actions on a server without a manifest: %v", bare)
	}

	// --json is the stored record, verbatim.
	raw := h.run("actions", "With manifest", "--json")
	var record struct {
		Revision int64 `json:"revision"`
		Manifest struct {
			KVNamespaces []string `json:"kvNamespaces"`
		} `json:"manifest"`
	}
	if raw.code != ExitOK || json.Unmarshal([]byte(raw.stdout), &record) != nil || record.Revision != 1 ||
		len(record.Manifest.KVNamespaces) != 1 {
		t.Errorf("actions --json: %v", raw)
	}
	listed := h.run("actions", "--json")
	var entries []struct {
		Server   map[string]any `json:"server"`
		Manifest map[string]any `json:"manifest"`
	}
	if listed.code != ExitOK || json.Unmarshal([]byte(listed.stdout), &entries) != nil || len(entries) != 2 {
		t.Fatalf("actions --json for every server: %v", listed)
	}
	manifests := 0
	for _, entry := range entries {
		if entry.Manifest != nil {
			manifests++
		}
	}
	if manifests != 1 {
		t.Errorf("actions --json: %d manifests, want 1 (and one null)", manifests)
	}
}

func TestHealth(t *testing.T) {
	t.Parallel()
	h := newTestHub(t)
	got := h.run("health")
	if got.code != ExitOK || !strings.Contains(got.stdout, "status:") || !strings.Contains(got.stdout, "ok") ||
		!strings.Contains(got.stdout, "sqlite (ok, schema") {
		t.Errorf("health: %v", got)
	}
	got = h.run("health", "--json")
	var health map[string]any
	if got.code != ExitOK || json.Unmarshal([]byte(got.stdout), &health) != nil || health["status"] != "ok" {
		t.Errorf("health --json: %v", got)
	}
}
