package main

import (
	"os/exec"
	"strings"
	"testing"
)

// The vyshka command is a client like any other: it reaches the hub through
// the public Admin API alone. Importing the hub, the panel, a plugin, or the
// conformance suites would let it lean on internals a third-party client
// cannot, and the command would stop proving the API is enough. Tests may
// import the hub to boot one; this checks the shipped binary's graph only.
func TestCommandImportsNoHubInternals(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	const module = "github.com/That1Drifter/vyshka/"
	forbidden := []string{"hub", "panel", "plugins", "conformance"}

	packages := strings.Fields(string(out))
	if len(packages) == 0 {
		t.Fatal("go list -deps printed nothing")
	}
	sawClient := false
	for _, path := range packages {
		if path == module+"client" {
			sawClient = true
		}
		for _, root := range forbidden {
			if path == module+root || strings.HasPrefix(path, module+root+"/") {
				t.Errorf("the vyshka command depends on %s", path)
			}
		}
	}
	// A check that lists nothing would pass forever; the client must be there.
	if !sawClient {
		t.Errorf("go list -deps does not include %sclient; is the check looking at the right package?", module)
	}
}
