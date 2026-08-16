package token_test

import (
	"strings"
	"testing"

	"github.com/That1Drifter/vyshka/hub/internal/token"
)

func TestNewIsPrefixedAndUnique(t *testing.T) {
	seen := make(map[string]bool, 1000)
	for range 1000 {
		value := token.New(token.Session)
		if !strings.HasPrefix(value, token.Session+"_") {
			t.Fatalf("token %q lacks its realm prefix", value)
		}
		if len(value) < 20 {
			t.Fatalf("token %q is too short to be a credential", value)
		}
		if seen[value] {
			t.Fatalf("token %q was minted twice", value)
		}
		seen[value] = true
	}
}

func TestHashIsStableAndOpaque(t *testing.T) {
	value := token.New(token.Admin)

	first, second := token.Hash(value), token.Hash(value)
	if first != second {
		t.Errorf("Hash is not stable: %q then %q", first, second)
	}
	if len(first) != 64 {
		t.Errorf("len(hash) = %d, want 64 hex characters", len(first))
	}
	if strings.Contains(first, value) {
		t.Error("the hash contains the token it is meant to hide")
	}
	if token.Hash(value) == token.Hash(value+"x") {
		t.Error("different tokens hashed alike")
	}
}

func TestEqual(t *testing.T) {
	value := token.New(token.Server)

	if !token.Equal(token.Hash(value), token.Hash(value)) {
		t.Error("Equal rejected two hashes of the same token")
	}
	if token.Equal(token.Hash(value), token.Hash(token.New(token.Server))) {
		t.Error("Equal accepted hashes of different tokens")
	}
	if token.Equal("", token.Hash(value)) {
		t.Error("Equal accepted an empty hash")
	}
}
