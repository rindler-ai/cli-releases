package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRevokeSelfClassifies(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	outcome, err := revokeSelf(context.Background(), srv.Client(), srv.URL, "rindler_live_k")
	if err != nil || outcome != revokeDone {
		t.Fatalf("2xx should revoke: outcome=%v err=%v", outcome, err)
	}
	if gotAuth != "Bearer rindler_live_k" || gotPath != "/api/cli/logout" {
		t.Errorf("auth=%q path=%q", gotAuth, gotPath)
	}

	// A 404 means the endpoint is not deployed. That must be reported as
	// not-revoked rather than an error, so logout still clears the local state
	// instead of aborting and leaving a configured-but-dead CLI.
	gone := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer gone.Close()
	outcome, err = revokeSelf(context.Background(), gone.Client(), gone.URL, "k")
	if err != nil || outcome != revokeUnreachable {
		t.Errorf("404 should be (false, nil), got outcome=%v err=%v", outcome, err)
	}
}

// Logout must clear local state even when the server cannot be reached —
// otherwise a user on a plane can never remove a key from their machine.
func TestLogoutClearsLocalStateWhenServerIsUnreachable(t *testing.T) {
	dir := isolate(t)
	os.Unsetenv("RINDLER_API_KEY")

	// Establish a logged-in-looking machine: stored key + config + MCP install.
	store, _, err := newCredentialStore()
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if err := store.setKey("rindler_live_stored"); err != nil {
		t.Fatalf("setKey: %v", err)
	}
	if err := saveConfig(cliConfig{
		// An origin that cannot resolve, so the revoke leg definitely fails.
		APIBase: "http://127.0.0.1:1", Last4: "ored", MCPURL: "http://127.0.0.1:1/mcp",
	}); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	// No agent config is planted: the CLI no longer installs one. What this
	// test still proves is the part that matters -- an unreachable revoke leg
	// must not stop logout clearing the local key and config.

	if code := run([]string{"logout"}); code != 0 {
		t.Fatalf("logout should exit 0 even when the revoke call fails, got %d", code)
	}

	if k, _ := store.getKey(); k != "" {
		t.Errorf("logout must clear the stored key, still have %q", k)
	}
	cfg, _ := loadConfig()
	if cfg.Last4 != "" {
		t.Errorf("logout must clear the config, still have %+v", cfg)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "claude", ".claude.json")); err == nil {
		if strings.Contains(string(b), "rindler_live_stored") {
			t.Errorf("logout must remove the key from the agent config, got %s", b)
		}
	}
}

func TestEnvOr(t *testing.T) {
	t.Setenv("RINDLER_TEST_ENVOR", "")
	if got := envOr("RINDLER_TEST_ENVOR", "fallback"); got != "fallback" {
		t.Errorf("empty env should fall back, got %q", got)
	}
	t.Setenv("RINDLER_TEST_ENVOR", "set")
	if got := envOr("RINDLER_TEST_ENVOR", "fallback"); got != "set" {
		t.Errorf("set env should win, got %q", got)
	}
}

func TestMapNote(t *testing.T) {
	if got := mapNote(true); got != " (runtime + mapping)" {
		t.Errorf("mapNote(true) = %q", got)
	}
	if got := mapNote(false); got != " (runtime)" {
		t.Errorf("mapNote(false) = %q", got)
	}
}

func TestOAuthErrorMessage(t *testing.T) {
	if got := (oauthError{Err: "invalid_grant", Desc: "expired"}).Error(); got != "invalid_grant: expired" {
		t.Errorf("got %q", got)
	}
	if got := (oauthError{Err: "invalid_grant"}).Error(); got != "invalid_grant" {
		t.Errorf("got %q", got)
	}
}

func TestResolveKeyAndBaseQuietPrefersFlagThenConfig(t *testing.T) {
	isolate(t)
	t.Setenv("RINDLER_API_KEY", "rindler_live_env")

	key, base, code := resolveKeyAndBaseQuiet("https://flag.example")
	if code != 0 || key != "rindler_live_env" || base != "https://flag.example" {
		t.Fatalf("flag should win: key=%q base=%q code=%d", key, base, code)
	}

	if err := saveConfig(cliConfig{APIBase: "https://cfg.example"}); err != nil {
		t.Fatal(err)
	}
	if _, base, _ = resolveKeyAndBaseQuiet(""); base != "https://cfg.example" {
		t.Errorf("config should be used when no flag, got %q", base)
	}

	if err := saveConfig(cliConfig{}); err != nil {
		t.Fatal(err)
	}
	if _, base, _ = resolveKeyAndBaseQuiet(""); base != defaultAPIBase {
		t.Errorf("should fall back to the default origin, got %q", base)
	}
}

func TestResolveKeyAndBaseQuietFailsLoggedOut(t *testing.T) {
	isolate(t)
	os.Unsetenv("RINDLER_API_KEY")
	if _, _, code := resolveKeyAndBaseQuiet(""); code == 0 {
		t.Error("logged out should not resolve a key")
	}
}

// The server answers 200 {"ok":true,"revoked":<bool>}, and `revoked` is false
// whenever there was nothing live to retire -- the key had already lapsed with
// its Clerk session. THREE outcomes, because two of them are successes and
// conflating them makes one read as a failure.
func TestLogoutTellsTheTwoSuccessesApart(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		want       revokeOutcome
	}{
		{"revoked a live key", `{"ok":true,"revoked":true}`, 200, revokeDone},
		// The DOMINANT case after a few days away. A success, not a warning.
		{"nothing left to revoke", `{"ok":true,"revoked":false}`, 200, revokeNothingToDo},
		// A 2xx with no discriminator: treat as done, since the difference only
		// changes a sentence.
		{"2xx with no revoked field", `{"ok":true}`, 200, revokeDone},
		{"2xx with no body at all", ``, 204, revokeDone},
		{"unparseable body", `<html>ok</html>`, 200, revokeDone},
		// Not deployed, or refused: we could not tell the server.
		{"endpoint absent", ``, 404, revokeUnreachable},
		{"server error", ``, 500, revokeUnreachable},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
			if tc.body != "" {
				_, _ = w.Write([]byte(tc.body))
			}
		}))
		got, err := revokeSelf(context.Background(), srv.Client(), srv.URL, "rindler_live_k")
		srv.Close()
		if err != nil {
			t.Errorf("%s: unexpected error %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("%s: outcome %v, want %v", tc.name, got, tc.want)
		}
	}
}
