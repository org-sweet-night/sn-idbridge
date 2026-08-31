// SPDX-License-Identifier: MIT

package clientsecret

import "testing"

func TestHashAndMatches(t *testing.T) {
	secret := "directory-reader-secret-1234567890"
	verifier := Hash(secret)
	if verifier == secret {
		t.Fatal("Hash() returned plaintext")
	}
	if !Valid(verifier) {
		t.Fatalf("Hash() = %q, want canonical verifier", verifier)
	}
	if !Matches(verifier, secret) {
		t.Fatal("Matches() = false for matching secret")
	}
	if Matches(verifier, "wrong-secret") {
		t.Fatal("Matches() = true for wrong secret")
	}
}

func TestMatchesRejectsPlaintextAndUnknownVerifierFormats(t *testing.T) {
	for _, verifier := range []string{
		"directory-reader-secret-1234567890",
		"",
		"sha256:not-hex",
		"md5:d41d8cd98f00b204e9800998ecf8427e",
	} {
		if Matches(verifier, verifier) {
			t.Fatalf("Matches(%q) accepted a noncanonical verifier", verifier)
		}
	}
}
