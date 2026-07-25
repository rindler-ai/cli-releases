//go:build !darwin && !linux

package main

import "errors"

// keyringService keeps the constant defined on every platform for reference.
const keyringService = "rindler-cli"

// newSystemBackend has no native keychain wiring on this OS yet (Windows
// Credential Manager / DPAPI is a follow-up), so the CLI falls back to the 0600
// file credential store with a warning (newCredentialStore).
func newSystemBackend() (keyringBackend, error) {
	return nil, errors.New("no native keychain backend for this OS yet")
}
