// Package token mints and verifies the bearer credentials of both API realms:
// enrollment tokens, server secrets, session tokens, and admin tokens.
//
// Every token is 26 base32 characters from crypto/rand (about 130 bits) behind
// a short realm prefix. The prefix exists so that a credential pasted into the
// wrong config file is obvious to a human; the protocol treats tokens as
// opaque (spec/protocol.md section 2.1) and nothing here parses one.
//
// Tokens are stored as SHA-256 digests. A password hash (bcrypt, argon2) buys
// nothing against a uniformly random 130-bit secret: there is no dictionary to
// try, and the digest is what the hub looks credentials up by.
package token

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
)

// Realm prefixes. They are a human affordance, never a parsing contract.
const (
	Enrollment = "vye" // one-time enrollment token
	Server     = "vys" // permanent per-server secret
	Session    = "vyt" // short-lived session token
	Admin      = "vya" // Admin API bearer token
	Webhook    = "vyw" // webhook signing secret
)

// New returns a fresh token in the given realm.
func New(realm string) string {
	return realm + "_" + rand.Text()
}

// Hash returns the stored form of a token: lowercase hex of its SHA-256.
func Hash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// Equal compares two hashes without leaking their difference through timing.
func Equal(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
