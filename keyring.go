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
// file with an explicit warning (never silent plaintext — the gh trap).
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
	// Named entries hold secrets other than the MCP key -- today the credential
	// vault's master key. Kept on the same interface so the vault inherits the
	// keyring-then-0600-file degradation instead of inventing a second one.
	setNamed(name, value string) error
	getNamed(name string) (string, error) // "" + nil when absent
	delNamed(name string) error
}

// allCredentialStores returns EVERY backend a key could be sitting in, not just
// the one this run prefers. Which backend newCredentialStore picks depends on an
// external binary being on PATH, so a key written by an earlier run can be in the
// other one. Logout must sweep both: revoking only the preferred store leaves a
// live key on disk while printing success.
func allCredentialStores() ([]credentialStore, error) {
	dir, err := configDir()
	if err != nil {
		return nil, err
	}
	stores := []credentialStore{&fileCredStore{path: filepath.Join(dir, "credentials.json")}}
	if kb, kerr := newSystemBackend(); kerr == nil {
		// Keyring first: it is the preferred home when it exists.
		stores = append([]credentialStore{&keyringCredStore{kb: kb}}, stores...)
	}
	return stores, nil
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

func (s *keyringCredStore) setNamed(name, value string) error { return s.kb.set(name, value) }
func (s *keyringCredStore) getNamed(name string) (string, error) {
	v, err := s.kb.get(name)
	if errors.Is(err, errNoEntry) {
		return "", nil
	}
	return v, err
}
func (s *keyringCredStore) delNamed(name string) error {
	err := s.kb.del(name)
	if errors.Is(err, errNoEntry) {
		return nil
	}
	return err
}

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

// Named entries live in sibling 0600 files rather than inside credentials.json,
// so `rindler logout` deleting the key file cannot take the vault key with it --
// losing that key would render every stored credential permanently unreadable.
func (s *fileCredStore) namedPath(name string) string {
	return filepath.Join(filepath.Dir(s.path), "named-"+name+".json")
}
func (s *fileCredStore) setNamed(name, value string) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(fileCred{Key: value})
	if err != nil {
		return err
	}
	return os.WriteFile(s.namedPath(name), b, 0o600)
}
func (s *fileCredStore) getNamed(name string) (string, error) {
	b, err := os.ReadFile(s.namedPath(name))
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
func (s *fileCredStore) delNamed(name string) error {
	if err := os.Remove(s.namedPath(name)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

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
