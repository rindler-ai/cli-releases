package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// End-to-end `rindler login`, with the browser stubbed. This is the whole login
// machinery under test — PKCE challenge, the loopback listener, the redirect
// capture, the token exchange, the keyring write, the config write, and the MCP
// install — against a server that speaks the real /api/cli/token contract.
//
// It stops short of Clerk itself: the consent page is a human signing in. What it
// proves is that everything AROUND that is correct, which is the part that can
// regress silently.

// fakeAuthServer plays the consent page and the token endpoint. On the consent
// GET it immediately redirects to the CLI's loopback redirect_uri with a code,
// which is exactly what the real dashboard does after a human approves.
func fakeAuthServer(t *testing.T, mapperAccess bool, onAuthorize func(q url.Values)) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /cli/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if onAuthorize != nil {
			onAuthorize(q)
		}
		redirect := q.Get("redirect_uri")
		state := q.Get("state")
		http.Redirect(w, r, redirect+"?code=test-code&state="+url.QueryEscape(state), http.StatusFound)
	})
	mux.HandleFunc("POST /api/cli/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.FormValue("code") != "test-code" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		if r.FormValue("code_verifier") == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_request","error_description":"missing verifier"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "rindler_live_fromlogin",
			"token_type":    "bearer",
			"last4":         "ogin",
			"expires_at":    "2099-01-01T00:00:00Z",
			"mapper_access": mapperAccess,
			"clerk_user_id": "user_test123",
			"mcp_url":       "https://mcp.example/mcp",
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// forceLoopback makes browserLikelyAvailable() true. Without it a CI box (no
// DISPLAY, or SSH_CONNECTION set) silently takes the PASTE path, which blocks on
// stdin — that is how these tests first failed.
func forceLoopback(t *testing.T) {
	t.Helper()
	t.Setenv("DISPLAY", ":0")
	t.Setenv("SSH_CONNECTION", "")
	t.Setenv("SSH_TTY", "")
}

// stubBrowser makes the "browser" fetch the consent URL, following the redirect
// back into the CLI's loopback listener.
func stubBrowser(t *testing.T) {
	t.Helper()
	orig := browserOpener
	browserOpener = func(u string) error {
		go func() {
			resp, err := http.Get(u)
			if err == nil {
				_ = resp.Body.Close()
			}
		}()
		return nil
	}
	t.Cleanup(func() { browserOpener = orig })
}

func TestLoginEndToEndStoresKeyAndInstallsMCP(t *testing.T) {
	dir := isolate(t)
	os.Unsetenv("RINDLER_API_KEY")
	forceLoopback(t)
	stubBrowser(t)

	var gotQuery url.Values
	srv := fakeAuthServer(t, true, func(q url.Values) { gotQuery = q })

	code := run([]string{"login", "--api-base", srv.URL, "--authorize-base", srv.URL})
	if code != 0 {
		t.Fatalf("login should exit 0, got %d", code)
	}

	// Mapping is requested BY DEFAULT — the regression this guards is a plain
	// login minting a key that is affirmatively denied the mapper.
	if got := gotQuery.Get("mapping_requested"); got != "true" && got != "1" {
		t.Errorf("login should request mapping by default, authorize query was %v", gotQuery)
	}
	// PKCE must be sent as a challenge, never the raw verifier.
	if gotQuery.Get("code_challenge") == "" {
		t.Error("authorize URL must carry a PKCE code_challenge")
	}
	if gotQuery.Get("code_challenge_method") != "S256" {
		t.Errorf("PKCE method should be S256, got %q", gotQuery.Get("code_challenge_method"))
	}
	// RFC 8252: the redirect must be literal loopback, never localhost.
	redirect := gotQuery.Get("redirect_uri")
	if !strings.HasPrefix(redirect, "http://127.0.0.1:") {
		t.Errorf("redirect_uri must be literal loopback, got %q", redirect)
	}

	store, _, err := newCredentialStore()
	if err != nil {
		t.Fatal(err)
	}
	if k, _ := store.getKey(); k != "rindler_live_fromlogin" {
		t.Errorf("login must store the minted key, got %q", k)
	}
	cfg, _ := loadConfig()
	if cfg.ClerkUserID != "user_test123" || !cfg.MapperAccess || cfg.Last4 != "ogin" {
		t.Errorf("config not persisted from the token response: %+v", cfg)
	}

	// And the MCP is installed with that key, which is the point of logging in.
	b, err := os.ReadFile(filepath.Join(dir, "claude", ".claude.json"))
	if err != nil {
		t.Fatalf("MCP not installed into Claude Code: %v", err)
	}
	if !strings.Contains(string(b), "rindler_live_fromlogin") {
		t.Errorf("agent config should carry the new key, got %s", b)
	}
	if !strings.Contains(string(b), "https://mcp.example/mcp") {
		t.Errorf("agent config should use the server-returned mcp_url, got %s", b)
	}
}

func TestLoginNoMapDoesNotRequestMapping(t *testing.T) {
	isolate(t)
	os.Unsetenv("RINDLER_API_KEY")
	forceLoopback(t)
	stubBrowser(t)
	var gotQuery url.Values
	srv := fakeAuthServer(t, false, func(q url.Values) { gotQuery = q })

	if code := run([]string{"login", "--no-map", "--no-mcp", "--api-base", srv.URL, "--authorize-base", srv.URL}); code != 0 {
		t.Fatalf("login --no-map should exit 0, got %d", code)
	}
	if got := gotQuery.Get("mapping_requested"); got == "true" || got == "1" {
		t.Errorf("--no-map must not request mapping, query was %v", gotQuery)
	}
}

func TestLoginNoMCPSkipsAgentInstall(t *testing.T) {
	dir := isolate(t)
	os.Unsetenv("RINDLER_API_KEY")
	forceLoopback(t)
	stubBrowser(t)
	srv := fakeAuthServer(t, true, nil)

	if code := run([]string{"login", "--no-mcp", "--api-base", srv.URL, "--authorize-base", srv.URL}); code != 0 {
		t.Fatalf("login --no-mcp should exit 0, got %d", code)
	}
	if _, err := os.ReadFile(filepath.Join(dir, "claude", ".claude.json")); err == nil {
		t.Error("--no-mcp must not write an agent config")
	}
	// The key is still stored — --no-mcp is about the agent install, not the login.
	store, _, _ := newCredentialStore()
	if k, _ := store.getKey(); k == "" {
		t.Error("--no-mcp must still store the key")
	}
}

// A rejected exchange must fail loudly and leave NOTHING behind, or the next
// command reads a half-written state and reports a confusing error.
func TestLoginFailureStoresNothing(t *testing.T) {
	isolate(t)
	os.Unsetenv("RINDLER_API_KEY")
	forceLoopback(t)
	stubBrowser(t)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /cli/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		http.Redirect(w, r, q.Get("redirect_uri")+"?code=bad&state="+url.QueryEscape(q.Get("state")), http.StatusFound)
	})
	mux.HandleFunc("POST /api/cli/token", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"code is invalid"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	if code := run([]string{"login", "--api-base", srv.URL, "--authorize-base", srv.URL}); code == 0 {
		t.Fatal("a rejected token exchange must not exit 0")
	}
	store, _, _ := newCredentialStore()
	if k, _ := store.getKey(); k != "" {
		t.Errorf("a failed login must not store a key, got %q", k)
	}
}

// CSRF: a callback carrying the wrong state must be refused. Without this an
// attacker who can reach the loopback port could inject their own code.
func TestLoginRejectsMismatchedState(t *testing.T) {
	isolate(t)
	os.Unsetenv("RINDLER_API_KEY")
	forceLoopback(t)
	stubBrowser(t)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /cli/authorize", func(w http.ResponseWriter, r *http.Request) {
		// Deliberately wrong state.
		http.Redirect(w, r, r.URL.Query().Get("redirect_uri")+"?code=test-code&state=attacker", http.StatusFound)
	})
	mux.HandleFunc("POST /api/cli/token", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "rindler_live_should_not_happen"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	code := run([]string{"login", "--timeout", "3s", "--api-base", srv.URL, "--authorize-base", srv.URL})
	if code == 0 {
		t.Fatal("a state mismatch must not produce a successful login")
	}
	store, _, _ := newCredentialStore()
	if k, _ := store.getKey(); k != "" {
		t.Errorf("a CSRF-mismatched callback must not store a key, got %q", k)
	}
}

// The paste flow is what a headless box or an SSH session actually gets, and it
// is selected AUTOMATICALLY there — so it is not an exotic path, it is the
// default for a whole class of users. pasteLogin takes its prompt as an argument,
// so it is drivable without a terminal.
func TestPasteLoginExchangesTheCodeAndVerifiesState(t *testing.T) {
	isolate(t)
	srv := fakeAuthServer(t, true, nil)

	p, err := newPKCE()
	if err != nil {
		t.Fatal(err)
	}
	opts := loginOpts{AuthorizeBase: srv.URL, APIBase: srv.URL, Mapping: true, Device: "test"}

	var shownURL string
	openFn := func(u string) error { shownURL = u; return nil }
	// The user pastes back "code#state", which the flow must split and verify.
	prompt := func(string) (string, error) { return "test-code#" + p.State, nil }

	tr, err := pasteLogin(t.Context(), opts, p, srv.Client(), openFn, prompt)
	if err != nil {
		t.Fatalf("pasteLogin errored: %v", err)
	}
	if tr.AccessToken != "rindler_live_fromlogin" {
		t.Errorf("token = %q", tr.AccessToken)
	}
	if shownURL == "" {
		t.Error("paste flow must show the user a URL to open")
	}

	// A mismatched state must be refused — this is the CSRF guard on the paste
	// lane, where the code travels through the user rather than a bound listener.
	badPrompt := func(string) (string, error) { return "test-code#attacker-state", nil }
	if _, err := pasteLogin(t.Context(), opts, p, srv.Client(), openFn, badPrompt); err == nil {
		t.Error("paste flow must reject a mismatched state")
	}
}
