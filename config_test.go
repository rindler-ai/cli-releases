package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestConfigRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RINDLER_CONFIG_DIR", dir)

	// Missing file => zero config, no error.
	got, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig empty: %v", err)
	}
	if got != (cliConfig{}) {
		t.Fatalf("expected zero config, got %+v", got)
	}

	want := cliConfig{
		Email:         "arthur@example.com",
		APIBase:       "https://mcp.rindler.ai",
		AuthorizeBase: "https://app.rindler.ai",
		MCPURL:        "https://mcp.rindler.ai/mcp",
		Last4:         "beef",
		ExpiresAt:     "2026-09-01T00:00:00Z",
		MapperAccess:  true,
	}
	if err := saveConfig(want); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	// File perms must be 0600.
	info, err := os.Stat(filepath.Join(dir, configFileName))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("config file perm = %o, want 600", perm)
	}
	got, err = loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if got != want {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", got, want)
	}

	// clearConfig removes it; then load is zero again.
	if err := clearConfig(); err != nil {
		t.Fatalf("clearConfig: %v", err)
	}
	if got, _ := loadConfig(); got != (cliConfig{}) {
		t.Errorf("expected zero after clear, got %+v", got)
	}
}

func TestConfigDirPrecedence(t *testing.T) {
	t.Setenv("RINDLER_CONFIG_DIR", "/tmp/explicit")
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg")
	if d, _ := configDir(); d != "/tmp/explicit" {
		t.Errorf("RINDLER_CONFIG_DIR should win, got %q", d)
	}
	t.Setenv("RINDLER_CONFIG_DIR", "")
	if d, _ := configDir(); d != filepath.Join("/tmp/xdg", "rindler") {
		t.Errorf("XDG_CONFIG_HOME should be next, got %q", d)
	}
}

func TestExpiryStatus(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name        string
		exp         string
		wantExpired bool
		wantMsg     bool
	}{
		{"empty", "", false, false},
		{"far future", now.Add(30 * 24 * time.Hour).Format(time.RFC3339), false, false},
		{"within 3 days", now.Add(2 * 24 * time.Hour).Format(time.RFC3339), false, true},
		{"past", now.Add(-time.Hour).Format(time.RFC3339), true, true},
		{"unparseable", "not-a-date", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg, expired := cliConfig{ExpiresAt: tc.exp}.expiryStatus(now)
			if expired != tc.wantExpired {
				t.Errorf("expired = %v, want %v", expired, tc.wantExpired)
			}
			if (msg != "") != tc.wantMsg {
				t.Errorf("msg = %q, wantMsg %v", msg, tc.wantMsg)
			}
		})
	}
}
