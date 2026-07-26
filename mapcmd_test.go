package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNormalizeMapTarget(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"https://example.com", "https://example.com"},
		{"http://example.com/x", "http://example.com/x"},
		// A bare host is what a human types; assume https rather than reject it.
		{"example.com", "https://example.com"},
		{"  example.com/path  ", "https://example.com/path"},
	} {
		got, err := normalizeMapTarget(c.in)
		if err != nil {
			t.Fatalf("normalizeMapTarget(%q) errored: %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("normalizeMapTarget(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	for _, bad := range []string{"", "   ", "ftp://example.com", "file:///etc/passwd", "://"} {
		if got, err := normalizeMapTarget(bad); err == nil {
			t.Errorf("normalizeMapTarget(%q) = %q, want an error", bad, got)
		}
	}
}

func TestMapTerminal(t *testing.T) {
	for _, s := range []string{"complete", "COMPLETED", " success ", "done"} {
		done, ok := mapTerminal(s)
		if !done || !ok {
			t.Errorf("mapTerminal(%q) = (%v,%v), want terminal success", s, done, ok)
		}
	}
	for _, s := range []string{"error", "failed", "cancelled", "timeout"} {
		done, ok := mapTerminal(s)
		if !done || ok {
			t.Errorf("mapTerminal(%q) = (%v,%v), want terminal failure", s, done, ok)
		}
	}
	// Anything unrecognised must keep polling, never be read as a silent success.
	for _, s := range []string{"", "queued", "running", "crawling", "weird"} {
		if done, _ := mapTerminal(s); done {
			t.Errorf("mapTerminal(%q) reported terminal, want in-flight", s)
		}
	}
}

// A 403 here means "your key has no mapper access", which is the exact thing the
// login default exists to prevent. The message must say what to DO about it.
func TestMapAuthErrorIsActionable(t *testing.T) {
	if got := mapAuthError(http.StatusUnauthorized, "").Error(); !strings.Contains(got, "rindler login") {
		t.Errorf("401 message %q should point at `rindler login`", got)
	}
	got := mapAuthError(http.StatusForbidden, "").Error()
	for _, want := range []string{"rindler status", "entitled"} {
		if !strings.Contains(got, want) {
			t.Errorf("403 message %q should mention %q", got, want)
		}
	}
	if got := mapAuthError(http.StatusServiceUnavailable, "").Error(); !strings.Contains(got, "not available") {
		t.Errorf("503 message %q should say mapping is unavailable", got)
	}
}

func TestStartMapSendsBearerAndParsesJobID(t *testing.T) {
	var gotAuth, gotPath string
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(mapStartResponse{JobID: "job-123"})
	}))
	defer srv.Close()

	id, err := startMap(context.Background(), srv.Client(), srv.URL, "rindler_live_abc", "https://example.com", "fast")
	if err != nil {
		t.Fatalf("startMap errored: %v", err)
	}
	if id != "job-123" {
		t.Errorf("job id = %q, want job-123", id)
	}
	if gotAuth != "Bearer rindler_live_abc" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotPath != "/v1/runtime/map" {
		t.Errorf("path = %q, want /v1/runtime/map", gotPath)
	}
	if gotBody["url"] != "https://example.com" || gotBody["mode"] != "fast" {
		t.Errorf("body = %v", gotBody)
	}
}

func TestStartMapSurfacesForbidden(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"this endpoint requires an admin key"}`))
	}))
	defer srv.Close()

	_, err := startMap(context.Background(), srv.Client(), srv.URL, "k", "https://example.com", "fast")
	if err == nil || !strings.Contains(err.Error(), "cannot map") {
		t.Fatalf("want the actionable mapper-access error, got %v", err)
	}
}

// A 200 with no job id is a server contract break; it must not be mistaken for a
// started run, or `map status` would be told to follow an empty id.
func TestStartMapRejectsMissingJobID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	if _, err := startMap(context.Background(), srv.Client(), srv.URL, "k", "https://example.com", "fast"); err == nil {
		t.Fatal("want an error when the server returns no job id")
	}
}

func TestMapStatusParsesEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/runtime/map/status/job-123" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"status":"complete","message":"9 screens","envelope":{"domain":"example.com"}}`))
	}))
	defer srv.Close()

	st, err := mapStatus(context.Background(), srv.Client(), srv.URL, "k", "job-123")
	if err != nil {
		t.Fatalf("mapStatus errored: %v", err)
	}
	if st.Status != "complete" || st.Envelope.Domain != "example.com" {
		t.Errorf("status = %+v", st)
	}
}

func TestFollowMapReturnsNonZeroOnFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"error","message":"bot blocked"}`))
	}))
	defer srv.Close()

	if code := followMap(context.Background(), srv.Client(), srv.URL, "k", "job-1"); code == 0 {
		t.Fatal("a failed run must exit non-zero")
	}
}

func TestFollowMapReturnsZeroOnSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"complete","envelope":{"domain":"example.com"}}`))
	}))
	defer srv.Close()

	if code := followMap(context.Background(), srv.Client(), srv.URL, "k", "job-1"); code != 0 {
		t.Fatalf("a successful run must exit 0, got %d", code)
	}
}
