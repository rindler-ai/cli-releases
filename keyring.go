package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Credential storage for the rindler CLI. The rindler_live_ key is a
// secret and lives in the OS keyring when one is available (the custody-daemon
// exec-based pattern, service "rindler-cli"); otherwise it falls back to a 0600
// file with an explicit warning (never silent plaintext — the gh  trap).
// Non-secret metadata lives separately in config.json (config.go).

// keyringBackend is the minimal OS-keychain surface, satisfied by the
// per-platform exec backends (keyring_{linux,darwin,other}.go); tests inject a
// fake. get returns errNoEntry when the account is absent.
type keyringBackend interface {
	set(account, secret string) error
	get(account string) (string, error)
	del(account string) error
}

// errNoEntry is what a backend returns from get when the account is not stored.
var errNoEntry = errors.New("keyring: no entry")

// keyringAccount is the single account holding the active MCP key.
const keyringAccount = "mcp-key"

// credentialStore abstracts where the key is persisted (keyring or file).
type credentialStore interface {
	setKey(key string) error
	getKey() (string, error) // returns "" + nil when no key is stored
	delKey() error
	// location is a human description for status output.
	location() string
}

// newCredentialStore selects the OS keyring when available, else the 0600 file
// fallback. warning is non-empty when it fell back, so the caller can surface it.
func newCredentialStore() (store credentialStore, warning string, err error) {
	kb, kerr := newSystemBackend()
	if kerr == nil {
		return &keyringCredStore{kb: kb}, "", nil
	}
	dir, derr := configDir()
	if derr != nil {
		return nil, "", derr
	}
	return &fileCredStore{path: filepath.Join(dir, "credentials.json")},
		fmt.Sprintf("no OS keyring available (%v); storing the key in a 0600 file instead", kerr),
		nil
}

// keyringCredStore persists the key in the OS keyring.
type keyringCredStore struct{ kb keyringBackend }

func (s *keyringCredStore) setKey(key string) error { return s.kb.set(keyringAccount, key) }
func (s *keyringCredStore) getKey() (string, error) {
	v, err := s.kb.get(keyringAccount)
	if errors.Is(err, errNoEntry) {
		return "", nil
	}
	return v, err
}
func (s *keyringCredStore) delKey() error {
	err := s.kb.del(keyringAccount)
	if errors.Is(err, errNoEntry) {
		return nil
	}
	return err
}
func (s *keyringCredStore) location() string { return "OS keyring (service rindler-cli)" }

// fileCredStore persists the key in a 0600 JSON file (dir 0700).
type fileCredStore struct{ path string }

type fileCred struct {
	Key string `json:"key"`
}

func (s *fileCredStore) setKey(key string) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(fileCred{Key: key})
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, b, 0o600)
}
func (s *fileCredStore) getKey() (string, error) {
	b, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	var fc fileCred
	if err := json.Unmarshal(b, &fc); err != nil {
		return "", err
	}
	return fc.Key, nil
}
func (s *fileCredStore) delKey() error {
	if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
func (s *fileCredStore) location() string { return s.path + " (0600 file)" }

// resolveActiveKey returns the key the CLI should use, honoring the highest-
// precedence RINDLER_API_KEY env override (CI/headless lane, never persisted)
// before consulting the credential store. src describes where it came from.
func resolveActiveKey(store credentialStore) (key, src string, err error) {
	if env := os.Getenv("RINDLER_API_KEY"); env != "" {
		return env, "RINDLER_API_KEY env", nil
	}
	k, err := store.getKey()
	if err != nil {
		return "", "", err
	}
	return k, store.location(), nil
}
