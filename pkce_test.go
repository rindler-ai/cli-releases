package main

import (
	"crypto/sha256"
	"encoding/base64"
	"testing"
)

func TestNewPKCEShape(t *testing.T) {
	p, err := newPKCE()
	if err != nil {
		t.Fatalf("newPKCE: %v", err)
	}
	// 32 bytes -> 43-char unpadded base64url.
	if len(p.Verifier) != 43 {
		t.Errorf("verifier len = %d, want 43", len(p.Verifier))
	}
	if len(p.Challenge) != 43 {
		t.Errorf("challenge len = %d, want 43", len(p.Challenge))
	}
	if len(p.State) != 43 {
		t.Errorf("state len = %d, want 43", len(p.State))
	}
	if p.Verifier == p.State {
		t.Error("verifier and state must be independent")
	}
}

func TestChallengeMatchesServerContract(t *testing.T) {
	// Mirror the server's verification: challenge == base64url(sha256(verifier)).
	p, err := newPKCE()
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(p.Verifier))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if p.Challenge != want {
		t.Errorf("challenge %q != server-recomputed %q", p.Challenge, want)
	}
}

func TestTwoAttemptsDiffer(t *testing.T) {
	a, _ := newPKCE()
	b, _ := newPKCE()
	if a.Verifier == b.Verifier || a.State == b.State {
		t.Error("two attempts must not collide")
	}
}
