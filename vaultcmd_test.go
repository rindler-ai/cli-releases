package main

import (
	"os"
	"path/filepath"
	"testing"
)

// OFF must be the default, and "off" has to be a real state rather than a label:
// an unpaired machine is invisible to the dashboard and chat and cannot be asked
// for a secret by any session. Signing in must not turn it on, because enrolling
// a laptop as a credential custodian a remote session can call is a decision
// someone should make deliberately.
func TestVaultDefaultsOff(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	if vaultEnabled() {
		t.Fatal("a fresh machine must have the vault OFF")
	}

	// Storing a credential must NOT switch custody on. The record is written and
	// encrypted, and stays inert.
	if err := os.MkdirAll(filepath.Join(dir, "rindler"), 0o700); err != nil {
		t.Fatal(err)
	}
	key, _, err := vaultMasterKey()
	if err != nil {
		t.Fatalf("master key: %v", err)
	}
	nonce, cipher, err := vaultSeal(key, "example.com", vaultSecret{Username: "u", Password: "p"})
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if err := saveVault(vaultFile{Version: 1, Records: []vaultRecord{{
		Site: "example.com", Nonce: nonce, Cipher: cipher, CreatedAt: "now",
	}}}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if vaultEnabled() {
		t.Fatal("storing a credential must not enable the vault")
	}
	if got := storedCredentialCount(); got != 1 {
		t.Fatalf("stored credentials = %d, want 1 (present but inert)", got)
	}
}

// Serving is the capability that matters, so it must refuse while off. A relay
// that started anyway would be reachable by a session despite the switch.
func TestServeRefusesWhileVaultOff(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if code := runDeviceServe(nil); code == 0 {
		t.Fatal("device serve must refuse while the credential vault is off")
	}
}

// Turning it off with nothing paired is a no-op, not an error: a user checking
// the switch should not be told something went wrong.
func TestDisableWhenAlreadyOffIsANoop(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if code := runVaultDisable(); code != 0 {
		t.Fatalf("disabling an already-off vault returned %d, want 0", code)
	}
}

// Status must be readable on a machine with no state at all.
func TestStatusWorksOnAFreshMachine(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if code := runVaultStatus(); code != 0 {
		t.Fatalf("vault status returned %d, want 0", code)
	}
}
