package main

import (
	"context"
	"encoding/json"
	"errors"
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
	// 503 covers BOTH "no mapper on this deployment" and a transient fault
	// ("could not read mapping state"). With no body to go on, the message must
	// hedge rather than assert the first, which is what it used to do.
	got503 := mapAuthError(http.StatusServiceUnavailable, "").Error()
	if !strings.Contains(got503, "not be available") || !strings.Contains(got503, "briefly down") {
		t.Errorf("503 with no body should hedge between the two causes, got %q", got503)
	}
	// With a body, the server's own words win: telling someone their deployment
	// cannot map when the real answer was "could not read mapping state" sends
	// them to fix infrastructure that is fine.
	transient := mapAuthError(http.StatusServiceUnavailable, `{"error":"could not read mapping state"}`).Error()
	if !strings.Contains(transient, "could not read mapping state") {
		t.Errorf("the server's own 503 reason was discarded: %q", transient)
	}
}

// 409 is two different things told apart only by the body: a race the next poll
// resolves, and a real refusal. Aborting on the first turned a momentary race
// into a failed map for a run that was still going.
func TestMapConflictSeparatesARaceFromARefusal(t *testing.T) {
	var retryable *mapRetryableError

	race := mapAuthError(http.StatusConflict, `{"error":"mapping status changed; retry"}`)
	if !errors.As(race, &retryable) {
		t.Errorf("a retry-me came back as %T; the poll will abort a live run", race)
	}

	refusal := mapAuthError(http.StatusConflict, `{"error":"another mapping already owns this domain"}`)
	if errors.As(refusal, &retryable) {
		t.Error("a real refusal was classified as retryable; the poll would spin until timeout")
	}
	if !strings.Contains(refusal.Error(), "already owns this domain") {
		t.Errorf("the refusal lost its reason: %q", refusal)
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
	// The server's own diagnosis comes first, then our fix. Both must be there:
	// the diagnosis says what is wrong, the fix says what to do about it.
	if err == nil {
		t.Fatal("a 403 must be an error")
	}
	if !strings.Contains(err.Error(), "requires an admin key") {
		t.Errorf("the server's diagnosis was discarded: %v", err)
	}
	if !strings.Contains(err.Error(), "rindler status") {
		t.Errorf("the actionable fix was dropped: %v", err)
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
