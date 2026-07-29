package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBearerFromHeader(t *testing.T) {
	if got := bearerFromHeader("Bearer abc123"); got != "abc123" {
		t.Errorf("got %q", got)
	}
	if got := bearerFromHeader("bearer  spaced  "); got != "spaced" {
		t.Errorf("case/space: got %q", got)
	}
	if got := bearerFromHeader("Basic abc"); got != "" {
		t.Errorf("non-bearer must be empty, got %q", got)
	}
}

// A revoked/foreign key installed in an agent config must be reported, not
// treated as healthy: this is the "installed but never connects" case.
func TestInstalledKeyMismatchDetected(t *testing.T) {
	if !installedKeyMatches("k1", "k1") {
		t.Error("same key must match")
	}
	if installedKeyMatches("old", "new") {
		t.Error("different keys must NOT match")
	}
	// Unknown on either side is not a claimed disagreement.
	if !installedKeyMatches("", "new") || !installedKeyMatches("old", "") {
		t.Error("unknown must not be reported as a mismatch")
	}
}

func TestInstalledKeysReadFromRealConfigs(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("CODEX_HOME", dir)

	if _, err := installClaudeCode("https://mcp.example/mcp", "rindler_live_KEY1"); err != nil {
		t.Fatal(err)
	}
	if _, err := installCodex("https://mcp.example/mcp", "rindler_live_KEY1"); err != nil {
		t.Fatal(err)
	}
	if got := installedClaudeKey(); got != "rindler_live_KEY1" {
		t.Errorf("claude key = %q", got)
	}
	if got := installedCodexKey(); got != "rindler_live_KEY1" {
		t.Errorf("codex key = %q", got)
	}
}

// The atomic write must leave no partial file behind and must preserve mode.
func TestWriteFilePreservePermIsAtomic(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "cfg.json")
	if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeFilePreservePerm(p, []byte(`{"a":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if string(b) != `{"a":1}` {
		t.Errorf("content = %q", string(b))
	}
	info, _ := os.Stat(p)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("perm = %o", info.Mode().Perm())
	}
	// No stray temp files left in the directory.
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".rindler-") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

// The expired message must not assert expiry as fact: the server rolls the
// expiry forward, so the local snapshot is often wrong.
func TestExpiredMessageIsHonestAboutBeingLocal(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	msg, expired := cliConfig{ExpiresAt: now.Add(-time.Hour).Format(time.RFC3339)}.expiryStatus(now)
	if !expired {
		t.Fatal("should still report expired=true")
	}
	if !strings.Contains(msg, "this machine") || !strings.Contains(msg, "doctor") {
		t.Errorf("message should say it is a local snapshot and point at doctor: %q", msg)
	}
}
