package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// tokenStub returns a token endpoint that mints for code=="CODE".
func tokenStub(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.PostForm.Get("code") != "CODE" || r.PostForm.Get("code_verifier") == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"rindler_live_x","token_type":"bearer","last4":"ivex","expires_at":"2026-09-01T00:00:00Z","mcp_url":"https://mcp.example/mcp"}`))
	}))
}

func TestLoopbackLoginEndToEnd(t *testing.T) {
	srv := tokenStub(t)
	defer srv.Close()
	p, _ := newPKCE()
	opts := loginOpts{AuthorizeBase: "https://app.example", APIBase: srv.URL, Device: "test"}

	// Fake browser: read the authorize URL, then simulate the dashboard redirecting
	// the browser to the loopback callback with the code + the echoed state.
	openFn := func(authURL string) error {
		u, _ := url.Parse(authURL)
		redirect := u.Query().Get("redirect_uri")
		state := u.Query().Get("state")
		go func() {
			cb, _ := url.Parse(redirect)
			q := cb.Query()
			q.Set("code", "CODE")
			q.Set("state", state)
			cb.RawQuery = q.Encode()
			_, _ = http.Get(cb.String())
		}()
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tr, err := loopbackLogin(ctx, opts, p, srv.Client(), openFn)
	if err != nil {
		t.Fatalf("loopbackLogin: %v", err)
	}
	if tr.AccessToken != "rindler_live_x" || tr.MCPURL != "https://mcp.example/mcp" {
		t.Fatalf("bad token: %+v", tr)
	}
}

func TestLoopbackLoginStateMismatch(t *testing.T) {
	srv := tokenStub(t)
	defer srv.Close()
	p, _ := newPKCE()
	opts := loginOpts{AuthorizeBase: "https://app.example", APIBase: srv.URL, Device: "test"}
	openFn := func(authURL string) error {
		u, _ := url.Parse(authURL)
		redirect := u.Query().Get("redirect_uri")
		go func() {
			cb, _ := url.Parse(redirect)
			q := cb.Query()
			q.Set("code", "CODE")
			q.Set("state", "WRONG-STATE")
			cb.RawQuery = q.Encode()
			_, _ = http.Get(cb.String())
		}()
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := loopbackLogin(ctx, opts, p, srv.Client(), openFn)
	if err == nil || !strings.Contains(err.Error(), "state mismatch") {
		t.Fatalf("expected state mismatch, got %v", err)
	}
}

func TestPasteLoginEndToEnd(t *testing.T) {
	srv := tokenStub(t)
	defer srv.Close()
	p, _ := newPKCE()
	opts := loginOpts{AuthorizeBase: "https://app.example", APIBase: srv.URL, Device: "test"}
	openFn := func(string) error { return nil }
	prompt := func(string) (string, error) { return "CODE#" + p.State, nil }

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tr, err := pasteLogin(ctx, opts, p, srv.Client(), openFn, prompt)
	if err != nil {
		t.Fatalf("pasteLogin: %v", err)
	}
	if tr.AccessToken != "rindler_live_x" {
		t.Fatalf("bad token: %+v", tr)
	}
}

func TestPasteLoginStateMismatch(t *testing.T) {
	srv := tokenStub(t)
	defer srv.Close()
	p, _ := newPKCE()
	opts := loginOpts{AuthorizeBase: "https://app.example", APIBase: srv.URL, Device: "test"}
	prompt := func(string) (string, error) { return "CODE#not-the-state", nil }
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := pasteLogin(ctx, opts, p, srv.Client(), func(string) error { return nil }, prompt)
	if err == nil || !strings.Contains(err.Error(), "state mismatch") {
		t.Fatalf("expected state mismatch, got %v", err)
	}
}

func TestMCPEndpointFallback(t *testing.T) {
	if got := mcpEndpoint(cliConfig{MCPURL: "https://mcp.rindler.ai/mcp"}); got != "https://mcp.rindler.ai/mcp" {
		t.Errorf("prefer server url, got %q", got)
	}
	if got := mcpEndpoint(cliConfig{APIBase: "https://mcp.rindler.ai/"}); got != "https://mcp.rindler.ai/mcp" {
		t.Errorf("fallback, got %q", got)
	}
	// Empty config (e.g. RINDLER_API_KEY lane) => prod default host, never "/mcp".
	if got := mcpEndpoint(cliConfig{}); got != defaultAPIBase+"/mcp" {
		t.Errorf("empty config should use prod default, got %q", got)
	}
}

func TestRunDispatch(t *testing.T) {
	if run([]string{"version"}) != 0 {
		t.Error("version should exit 0")
	}
	if run([]string{"help"}) != 0 {
		t.Error("help should exit 0")
	}
	if run([]string{"bogus-cmd"}) != 2 {
		t.Error("unknown command should exit 2")
	}
	if run(nil) != 2 {
		t.Error("no args should exit 2")
	}
}
