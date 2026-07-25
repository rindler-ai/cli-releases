package main

import (
	"os"
	"path/filepath"
	"testing"
)

// fakeBackend is an in-memory keyringBackend.
type fakeBackend struct{ m map[string]string }

func newFakeBackend() *fakeBackend           { return &fakeBackend{m: map[string]string{}} }
func (f *fakeBackend) set(a, s string) error { f.m[a] = s; return nil }
func (f *fakeBackend) get(a string) (string, error) {
	v, ok := f.m[a]
	if !ok {
		return "", errNoEntry
	}
	return v, nil
}
func (f *fakeBackend) del(a string) error { delete(f.m, a); return nil }

func TestKeyringCredStore(t *testing.T) {
	s := &keyringCredStore{kb: newFakeBackend()}
	// Empty => "" + nil.
	if k, err := s.getKey(); err != nil || k != "" {
		t.Fatalf("empty get = %q, %v", k, err)
	}
	// del on empty is nil.
	if err := s.delKey(); err != nil {
		t.Fatalf("del empty: %v", err)
	}
	if err := s.setKey("rindler_live_abc123"); err != nil {
		t.Fatal(err)
	}
	if k, err := s.getKey(); err != nil || k != "rindler_live_abc123" {
		t.Fatalf("get = %q, %v", k, err)
	}
	if err := s.delKey(); err != nil {
		t.Fatal(err)
	}
	if k, _ := s.getKey(); k != "" {
		t.Fatalf("expected empty after del, got %q", k)
	}
}

func TestFileCredStore(t *testing.T) {
	dir := t.TempDir()
	s := &fileCredStore{path: filepath.Join(dir, "credentials.json")}
	if k, err := s.getKey(); err != nil || k != "" {
		t.Fatalf("empty get = %q, %v", k, err)
	}
	if err := s.setKey("rindler_live_xyz"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(s.path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("cred file perm = %o, want 600", perm)
	}
	if k, err := s.getKey(); err != nil || k != "rindler_live_xyz" {
		t.Fatalf("get = %q, %v", k, err)
	}
	if err := s.delKey(); err != nil {
		t.Fatal(err)
	}
	if k, _ := s.getKey(); k != "" {
		t.Errorf("expected empty after del, got %q", k)
	}
}

func TestResolveActiveKeyEnvWins(t *testing.T) {
	s := &keyringCredStore{kb: newFakeBackend()}
	_ = s.setKey("stored-key")

	t.Setenv("RINDLER_API_KEY", "env-key")
	k, src, err := resolveActiveKey(s)
	if err != nil || k != "env-key" {
		t.Fatalf("env should win: %q %q %v", k, src, err)
	}

	t.Setenv("RINDLER_API_KEY", "")
	k, _, err = resolveActiveKey(s)
	if err != nil || k != "stored-key" {
		t.Fatalf("store fallback: %q %v", k, err)
	}
}
