package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestBuildAuthorizeURL(t *testing.T) {
	p := pkce{Verifier: "v", Challenge: "chal", State: "st8"}
	raw := buildAuthorizeURL("https://app.rindler.ai/", "http://127.0.0.1:5/callback", p, "laptop (linux)", true, true)
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if u.Host != "app.rindler.ai" || u.Path != "/cli/authorize" {
		t.Errorf("bad host/path: %s", raw)
	}
	q := u.Query()
	checks := map[string]string{
		"response_type":         "code",
		"client_id":             "rindler-cli",
		"redirect_uri":          "http://127.0.0.1:5/callback",
		"scope":                 "mcp",
		"code_challenge":        "chal",
		"code_challenge_method": "S256",
		"state":                 "st8",
		"device":                "laptop (linux)",
		"mapping_requested":     "1",
		"paste":                 "1",
	}
	for k, want := range checks {
		if got := q.Get(k); got != want {
			t.Errorf("query %s = %q, want %q", k, got, want)
		}
	}
	// Without mapping/paste those params are absent.
	raw2 := buildAuthorizeURL("https://app.rindler.ai", "r", p, "d", false, false)
	q2, _ := url.Parse(raw2)
	if q2.Query().Has("mapping_requested") || q2.Query().Has("paste") {
		t.Error("mapping/paste should be absent when false")
	}
}

func TestDeviceLabel(t *testing.T) {
	if l := deviceLabel(); l == "" || !strings.Contains(l, "(") {
		t.Errorf("bad device label: %q", l)
	}
}

func TestSplitPastedCode(t *testing.T) {
	if c, s := splitPastedCode("  abc#xyz  "); c != "abc" || s != "xyz" {
		t.Errorf("got %q,%q", c, s)
	}
	if c, s := splitPastedCode("baremcode"); c != "baremcode" || s != "" {
		t.Errorf("bare: got %q,%q", c, s)
	}
}

func TestExchangeTokenSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.PostForm.Get("grant_type") != "authorization_code" || r.PostForm.Get("code") != "CODE" ||
			r.PostForm.Get("code_verifier") != "VERIFIER" || r.PostForm.Get("client_id") != "rindler-cli" {
			t.Errorf("bad form: %v", r.PostForm)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"rindler_live_tok","last4":"tok9","expires_at":"2026-09-01T00:00:00Z","mapper_access":true,"clerk_user_id":"user_1","mcp_url":"https://mcp.rindler.ai/mcp"}`))
	}))
	defer srv.Close()

	tr, err := exchangeToken(context.Background(), srv.Client(), srv.URL, "CODE", "VERIFIER", "http://127.0.0.1:5/callback")
	if err != nil {
		t.Fatal(err)
	}
	if tr.AccessToken != "rindler_live_tok" || !tr.MapperAccess || tr.MCPURL != "https://mcp.rindler.ai/mcp" {
		t.Errorf("bad response: %+v", tr)
	}
}

func TestExchangeTokenOAuthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid_grant","error_description":"code expired"}`))
	}))
	defer srv.Close()

	_, err := exchangeToken(context.Background(), srv.Client(), srv.URL, "c", "v", "r")
	var oe oauthError
	if !errors.As(err, &oe) || oe.Err != "invalid_grant" {
		t.Fatalf("expected oauthError invalid_grant, got %v", err)
	}
}

func TestExchangeTokenNonJSONError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`gateway blew up`))
	}))
	defer srv.Close()

	if _, err := exchangeToken(context.Background(), srv.Client(), srv.URL, "c", "v", "r"); err == nil {
		t.Fatal("expected error on 500")
	}
}

func TestExchangeTokenMissingToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"last4":"x"}`))
	}))
	defer srv.Close()

	if _, err := exchangeToken(context.Background(), srv.Client(), srv.URL, "c", "v", "r"); err == nil {
		t.Fatal("expected error when access_token missing")
	}
}
