// SPDX-License-Identifier: MIT

// Package clientsecret owns the persisted verifier format for confidential
// OIDC client secrets. Client secrets are generated with high entropy, so a
// deterministic SHA-256 verifier provides one-way storage while allowing the
// token exchange to recheck the current verifier atomically.
package clientsecret

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"strings"
)

const Prefix = "sha256:"

// Hash returns the canonical persisted verifier for a plaintext client secret.
func Hash(secret string) string {
	digest := sha256.Sum256([]byte(secret))
	return Prefix + hex.EncodeToString(digest[:])
}

// Matches reports whether candidate matches a well-formed canonical verifier.
// Plaintext and unknown verifier formats fail closed.
func Matches(verifier, candidate string) bool {
	if !Valid(verifier) {
		return false
	}
	candidateVerifier := Hash(candidate)
	return subtle.ConstantTimeCompare([]byte(verifier), []byte(candidateVerifier)) == 1
}

// Valid reports whether verifier has the canonical persisted format.
func Valid(verifier string) bool {
	if len(verifier) != len(Prefix)+sha256.Size*2 || !strings.HasPrefix(verifier, Prefix) {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(verifier, Prefix))
	return err == nil
}
