package client_test

import (
	"os"
	"regexp"
	"testing"

	"github.com/That1Drifter/vyshka/client"
)

// The client names the protocol draft it was written against, and `vyshka
// version` prints it. A protocol draft bump updates client.ProtocolDraft in
// the same commit, after checking that nothing the client models changed
// under it; this test fails until that happens.
func TestProtocolDraftMatchesTheSpec(t *testing.T) {
	spec, err := os.ReadFile("../spec/protocol.md")
	if err != nil {
		t.Fatalf("read the protocol document: %v", err)
	}
	match := regexp.MustCompile(`\*\*Status:\*\* draft (\d+\.\d+)`).FindSubmatch(spec)
	if match == nil {
		t.Fatal("spec/protocol.md carries no **Status:** draft line")
	}
	if got := string(match[1]); got != client.ProtocolDraft {
		t.Errorf("spec/protocol.md is draft %s, client.ProtocolDraft is %s", got, client.ProtocolDraft)
	}
}
