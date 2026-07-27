package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func usageServer(t *testing.T, scope string, status int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/workspace/usage/me", func(w http.ResponseWriter, r *http.Request) {
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		answered := scope
		if answered == "" {
			answered = r.URL.Query().Get("member")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"scope": answered,
			"credits": map[string]int{
				"remaining": 975, "used": 25, "allotment": 1000,
			},
			"credits_reconstructed": true,
			"sessions":              map[string]int{"total": 30, "refunded": 5},
			"sites": []map[string]any{
				{"domain": "example.com", "sessions": 20, "credits": 18, "last_worked_at": "2026-07-26"},
			},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestUsageReadsYourOwnNumbers(t *testing.T) {
	isolate(t)
	srv := usageServer(t, "me", http.StatusOK)
	t.Setenv("RINDLER_API_KEY", "rindler_live_test")
	if code := run([]string{"usage", "--api-base", srv.URL}); code != 0 {
		t.Fatalf("usage should exit 0, got %d", code)
	}
	if code := run([]string{"usage", "--json", "--api-base", srv.URL}); code != 0 {
		t.Fatalf("usage --json should exit 0, got %d", code)
	}
}

// The guard that matters. If the server answers a different question than the
// one asked, printing its numbers under our heading is a quiet lie, so this is
// an error rather than a render.
func TestUsageRefusesAScopeMismatch(t *testing.T) {
	isolate(t)
	// Asked for "me", answered "workspace".
	srv := usageServer(t, "workspace", http.StatusOK)
	t.Setenv("RINDLER_API_KEY", "rindler_live_test")
	if code := run([]string{"usage", "--api-base", srv.URL}); code == 0 {
		t.Fatal("a scope mismatch must not exit 0")
	}
	// Asking for the workspace and getting it back is fine.
	if code := run([]string{"usage", "--workspace", "--api-base", srv.URL}); code != 0 {
		t.Fatalf("matching scope should exit 0, got %d", code)
	}
}

func TestUsageSurfacesAnAuthFailure(t *testing.T) {
	isolate(t)
	srv := usageServer(t, "me", http.StatusUnauthorized)
	t.Setenv("RINDLER_API_KEY", "rindler_live_test")
	if code := run([]string{"usage", "--api-base", srv.URL}); code == 0 {
		t.Fatal("a 401 must not exit 0")
	}
}

// Both disclosures are true of the DATA, not the surface, so the CLI owes the
// reader the same sentences the dashboard shows.
func TestUsagePrintsBothDisclosures(t *testing.T) {
	var b strings.Builder
	u := usageResponse{CreditsReconstructed: true}
	u.Credits.Allotment = 1000
	u.Credits.Remaining = 975
	printUsage(&b, u, scopeMe)
	out := b.String()
	if !strings.Contains(out, creditsReconstructedNote) {
		t.Error("must disclose that per-member credits are reconstructed")
	}
	if !strings.Contains(out, alsoVisibleNote) {
		t.Error("must disclose that owners and admins can see the same numbers")
	}
}

// A bad total or an over-spend must not draw a negative or runaway bar.
func TestCreditBarIsClamped(t *testing.T) {
	for _, tc := range []struct{ remaining, total int }{
		{-5, 100}, {150, 100}, {50, 0}, {0, 0},
	} {
		got := creditBar(tc.remaining, tc.total)
		if len(got) > 24 || strings.Contains(got, "-1") {
			t.Fatalf("creditBar(%d,%d) = %q", tc.remaining, tc.total, got)
		}
	}
}
