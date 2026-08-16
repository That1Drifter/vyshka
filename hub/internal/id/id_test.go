package id_test

import (
	"strings"
	"testing"
	"time"

	"github.com/That1Drifter/vyshka/hub/internal/id"
)

func TestNewAtEncodesTheTimestamp(t *testing.T) {
	// Known answer: the ULID specification's example id
	// 01ARZ3NDEKTSV4RRFFQ69G5FAV carries the timestamp 1469922850259 ms, so
	// encoding that instant must reproduce its first ten characters.
	got := id.NewAt(time.UnixMilli(1469922850259))
	if len(got) != id.Length {
		t.Fatalf("len(%q) = %d, want %d", got, len(got), id.Length)
	}
	if prefix := got[:10]; prefix != "01ARZ3NDEK" {
		t.Errorf("timestamp prefix = %q, want 01ARZ3NDEK (full id %q)", prefix, got)
	}
}

func TestNewAtEncodesTheBoundaries(t *testing.T) {
	if got := id.NewAt(time.UnixMilli(0))[:10]; got != "0000000000" {
		t.Errorf("epoch prefix = %q, want 0000000000", got)
	}
	// The 48-bit timestamp saturates at 2^48-1 ms, which is the highest value
	// the first ten characters can carry.
	if got := id.NewAt(time.UnixMilli(1<<48 - 1))[:10]; got != "7ZZZZZZZZZ" {
		t.Errorf("max prefix = %q, want 7ZZZZZZZZZ", got)
	}
}

func TestNewAtSortsByTime(t *testing.T) {
	base := time.UnixMilli(1_700_000_000_000)

	previous := ""
	for offset := range 50 {
		current := id.NewAt(base.Add(time.Duration(offset) * time.Millisecond))
		if previous != "" && current <= previous {
			t.Fatalf("id %q does not sort after %q", current, previous)
		}
		previous = current
	}
}

func TestNewIsUniqueAndUsesCrockfordBase32(t *testing.T) {
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

	seen := make(map[string]bool, 10_000)
	for range 10_000 {
		value := id.New()
		if seen[value] {
			t.Fatalf("id %q was minted twice", value)
		}
		seen[value] = true

		if strings.ContainsAny(value, "ILOU") {
			t.Fatalf("id %q contains a character Crockford base32 excludes", value)
		}
		for _, character := range value {
			if !strings.ContainsRune(alphabet, character) {
				t.Fatalf("id %q contains %q, which is outside the alphabet", value, character)
			}
		}
	}
}
