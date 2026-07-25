package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
)

// PKCE (RFC 7636) + CSRF state generation for the CLI login flow. The
// server side (the server's PKCE verification) recomputes
// base64url(sha256(verifier)) and constant-time-compares it to the challenge, so
// these MUST use RawURLEncoding + SHA-256 to interoperate.

// pkce holds one login attempt's proof-of-possession material.
type pkce struct {
	Verifier  string // sent to /api/cli/token
	Challenge string // sent in the authorize URL (S256 of the verifier)
	State     string // CSRF nonce echoed back on both loopback and paste paths
}

// newPKCE generates a fresh verifier (32 random bytes -> 43-char base64url), its
// S256 challenge, and a 32-byte state nonce.
func newPKCE() (pkce, error) {
	verifier, err := randB64URL(32)
	if err != nil {
		return pkce{}, err
	}
	state, err := randB64URL(32)
	if err != nil {
		return pkce{}, err
	}
	return pkce{
		Verifier:  verifier,
		Challenge: challengeFromVerifier(verifier),
		State:     state,
	}, nil
}

// challengeFromVerifier is the S256 code_challenge for a verifier.
func challengeFromVerifier(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// randB64URL returns n cryptographically-random bytes as unpadded base64url.
func randB64URL(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
