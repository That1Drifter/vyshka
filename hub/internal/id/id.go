// Package id mints ULIDs: 48 bits of millisecond timestamp followed by 80 bits
// of randomness, rendered as 26 characters of Crockford base32.
//
// The protocol calls for ULIDs on envelopes (spec/protocol.md section 4) and
// uses the same shape for hub-assigned identifiers, so every id sorts by
// creation time and stays copy-pasteable into a URL or a log filter. It is
// implemented here rather than pulled in as a dependency because the whole of
// it is one encoding loop.
package id

import (
	"crypto/rand"
	"time"
)

// alphabet is Crockford base32: no I, L, O, or U, so an id read aloud or typed
// from a screenshot does not turn into a different id.
const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// Length is the character count of every id this package produces.
const Length = 26

// New returns a ULID stamped with the current time.
func New() string { return NewAt(time.Now()) }

// NewAt returns a ULID stamped with t. Ids minted in the same millisecond are
// ordered arbitrarily among themselves; nothing in the protocol relies on
// finer ordering than the timestamp gives.
func NewAt(t time.Time) string {
	var raw [16]byte

	ms := uint64(t.UTC().UnixMilli())
	for i := range 6 {
		raw[i] = byte(ms >> (40 - 8*i))
	}
	// crypto/rand.Read never returns an error; it crashes the program instead.
	_, _ = rand.Read(raw[6:])

	return encode(raw)
}

// encode renders the 128-bit value as 26 base32 characters. The value is read
// as if left-padded to 130 bits, so the first character carries only two
// significant bits and never exceeds '7'.
func encode(raw [16]byte) string {
	out := make([]byte, Length)
	for i := range out {
		var index byte
		for bit := range 5 {
			position := i*5 + bit - 2
			index <<= 1
			if position >= 0 && raw[position/8]&(0x80>>(position%8)) != 0 {
				index |= 1
			}
		}
		out[i] = alphabet[index]
	}
	return string(out)
}
