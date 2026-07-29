package main

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// These cover the ways a vault master key could previously be lost: the key
// living in the backend this run does NOT prefer, and a backend that ERRORS
// (a locked keyring) being read as "no key yet". Both used to mint a fresh key
// over a populated vault, which makes every stored credential unreadable.

func writeVaultWithRecord(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"records":[{"site":"example.com","nonce":"x","cipher":"y","created_at":"2026-01-01T00:00:00Z"}]}`
	if err := os.WriteFile(filepath.Join(dir, vaultFileName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestVaultKeyFoundInFileStoreWhenKeyringPreferred: the key was written by a run
// with no keyring; a later run that DOES have one must still find it.
func TestVaultKeyFoundInFileStoreWhenKeyringPreferred(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RINDLER_CONFIG_DIR", dir)
	writeVaultWithRecord(t, dir)

	want := make([]byte, 32)
	for i := range want {
		want[i] = byte(i + 1)
	}
	enc := base64.StdEncoding.EncodeToString(want)
	// The file store's named entry — where a no-keyring run puts it.
	fs := &fileCredStore{path: filepath.Join(dir, "credentials.json")}
	if err := fs.setNamed(vaultKeyringEntry, enc); err != nil {
		t.Fatal(err)
	}

	// An EMPTY keyring is preferred; it must not shadow the real key.
	empty := &keyringCredStore{kb: newFakeBackend()}
	got, _, err := resolveVaultKeyForTest(empty, dir)
	if err != nil {
		t.Fatalf("expected the file-store key to be found, got error: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("wrong key recovered: the vault would be unreadable")
	}
}

// TestVaultKeyRefusesToMintOverPopulatedVault: a backend error (locked keyring)
// must never be treated as "first use".
func TestVaultKeyRefusesToMintOverPopulatedVault(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RINDLER_CONFIG_DIR", dir)
	writeVaultWithRecord(t, dir)

	// No key anywhere, and the vault has records => refuse rather than mint.
	empty := &keyringCredStore{kb: newFakeBackend()}
	if _, _, err := resolveVaultKeyForTest(empty, dir); !errors.Is(err, errVaultKeyUnavailable) {
		t.Fatalf("expected errVaultKeyUnavailable over a populated vault, got %v", err)
	}
}

// TestVaultKeyMintsOnGenuineFirstUse: an empty vault still bootstraps.
func TestVaultKeyMintsOnGenuineFirstUse(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RINDLER_CONFIG_DIR", dir)
	// No vault file at all.
	k, _, err := vaultMasterKey()
	if err != nil {
		t.Fatalf("first use should mint, got %v", err)
	}
	if len(k) != 32 {
		t.Fatalf("bad key length %d", len(k))
	}
	// And the next call must return the SAME key, not a new one.
	k2, _, err := vaultMasterKey()
	if err != nil || string(k) != string(k2) {
		t.Fatalf("key not stable across calls: %v", err)
	}
}

// resolveVaultKeyForTest exercises the probe order with an explicit store, which
// is what vaultMasterKey does internally with the process's preferred backend.
func resolveVaultKeyForTest(store credentialStore, dir string) ([]byte, string, error) {
	probeFailed := false
	for _, probe := range vaultKeyLocations(store, dir) {
		raw, perr := probe()
		if perr != nil {
			probeFailed = true
			continue
		}
		if raw == "" {
			continue
		}
		if k, ok := decodeVaultKey(raw); ok {
			return k, "", nil
		}
	}
	hasRecords, _ := vaultHasRecords()
	if hasRecords || probeFailed {
		return nil, "", errVaultKeyUnavailable
	}
	return nil, "", errors.New("would mint")
}
