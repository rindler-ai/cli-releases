package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseInputs(t *testing.T) {
	got, err := parseInputs([]string{"query=shoes", "color=blue"})
	if err != nil {
		t.Fatalf("parseInputs errored: %v", err)
	}
	if got["query"] != "shoes" || got["color"] != "blue" {
		t.Errorf("parseInputs = %v", got)
	}
	// An empty value is legitimate ("clear this field"), a missing '=' is not.
	if got, err := parseInputs([]string{"query="}); err != nil || got["query"] != "" {
		t.Errorf("empty value should be allowed, got %v err %v", got, err)
	}
	for _, bad := range []string{"query", "=shoes", " =x"} {
		if _, err := parseInputs([]string{bad}); err == nil {
			t.Errorf("parseInputs(%q) should refuse rather than silently drop the input", bad)
		}
	}
}

func TestSiteFromTarget(t *testing.T) {
	for in, want := range map[string]string{
		"example.com":              "example.com",
		"https://example.com":      "example.com",
		"https://example.com/path": "example.com",
		"  example.com  ":          "example.com",
	} {
		got, err := siteFromTarget(in)
		if err != nil || got != want {
			t.Errorf("siteFromTarget(%q) = %q,%v want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "   "} {
		if _, err := siteFromTarget(bad); err == nil {
			t.Errorf("siteFromTarget(%q) should error", bad)
		}
	}
}

func TestRunTerminal(t *testing.T) {
	for _, s := range []string{"complete", "SUCCEEDED", " success "} {
		if done, ok := runTerminal(s); !done || !ok {
			t.Errorf("runTerminal(%q) should be terminal success", s)
		}
	}
	for _, s := range []string{"failed", "error", "cancelled", "timed_out"} {
		if done, ok := runTerminal(s); !done || ok {
			t.Errorf("runTerminal(%q) should be terminal failure", s)
		}
	}
	// An unknown status must keep polling. Reading it as terminal would end the
	// follow early and report partial state as the answer.
	for _, s := range []string{"", "queued", "running", "something_new"} {
		if done, _ := runTerminal(s); done {
			t.Errorf("runTerminal(%q) should be non-terminal", s)
		}
	}
}

func TestNewIdempotencyKeyIsUnique(t *testing.T) {
	a, b := newIdempotencyKey(), newIdempotencyKey()
	if a == b || a == "" {
		t.Fatalf("idempotency keys must be unique and non-empty (%q, %q)", a, b)
	}
}

func TestStartRunSendsContract(t *testing.T) {
	var body map[string]any
	var auth, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, path = r.Header.Get("Authorization"), r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"job_id":"j1"}`))
	}))
	defer srv.Close()

	id, err := startRun(context.Background(), srv.Client(), srv.URL, "k", "example.com",
		[]string{"search_products"}, map[string]string{"query": "shoes"}, "", 0)
	if err != nil {
		t.Fatalf("startRun errored: %v", err)
	}
	if id != "j1" {
		t.Errorf("job id = %q", id)
	}
	if auth != "Bearer k" || path != "/v1/runtime/run" {
		t.Errorf("auth=%q path=%q", auth, path)
	}
	if body["site"] != "example.com" {
		t.Errorf("site = %v", body["site"])
	}
	// idempotency_key is REQUIRED by the server; omitting it 400s every call.
	if s, _ := body["idempotency_key"].(string); s == "" {
		t.Error("idempotency_key must always be sent")
	}
	acts, _ := body["actions"].([]any)
	if len(acts) != 1 || acts[0] != "search_products" {
		t.Errorf("actions = %v", body["actions"])
	}
}

// Run accepts ANY key, so a 403 is about the SITE. Telling the user to log in
// again would send them to fix something that is not broken.
func TestRunAuthErrorsAreSpecific(t *testing.T) {
	if got := runAuthError(http.StatusForbidden, "").Error(); strings.Contains(got, "rindler login") {
		t.Errorf("403 should not blame the login: %q", got)
	}
	if got := runAuthError(http.StatusForbidden, "").Error(); !strings.Contains(got, "catalog") {
		t.Errorf("403 should explain the site is not accessible: %q", got)
	}
	if got := runAuthError(http.StatusNotFound, "").Error(); !strings.Contains(got, "rindler map") {
		t.Errorf("404 should point at mapping the site: %q", got)
	}
	if got := runAuthError(http.StatusUnauthorized, "").Error(); !strings.Contains(got, "rindler login") {
		t.Errorf("401 should point at login: %q", got)
	}
}

func jobServer(t *testing.T, envelope string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(envelope))
	}))
}

func TestFollowRunSucceedsOnCompleteWithRecords(t *testing.T) {
	srv := jobServer(t, `{"status":"complete","usage":{"outcome_count":2},
	  "outputs":{"records":[{"title":"A"},{"title":"B"}]},
	  "retrieval":{"outcome":"records","complete":true}}`)
	defer srv.Close()
	if code := followRun(context.Background(), srv.Client(), srv.URL, "k", "j1", false, ""); code != 0 {
		t.Fatalf("a complete run with records must exit 0, got %d", code)
	}
}

// THE case the server split status and retrieval to expose: the job ran fine,
// but a bot wall meant nothing usable came back. Exiting 0 here would tell a
// script the task succeeded.
func TestFollowRunFailsWhenRetrievalIsIncomplete(t *testing.T) {
	srv := jobServer(t, `{"status":"complete","usage":{"outcome_count":0},
	  "outputs":{"records":[]},
	  "retrieval":{"outcome":"blocked","complete":false,"failure_shape":"bot_wall",
	               "reasons":["challenge page served"],"retry_guidance":"retry via a stealth tier"}}`)
	defer srv.Close()
	if code := followRun(context.Background(), srv.Client(), srv.URL, "k", "j1", false, ""); code == 0 {
		t.Fatal("a job that ran but retrieved nothing usable must NOT exit 0")
	}
}

func TestFollowRunFailsOnFailedStatus(t *testing.T) {
	srv := jobServer(t, `{"status":"failed","error_msg":"selector gone","usage":{"outcome_count":0}}`)
	defer srv.Close()
	if code := followRun(context.Background(), srv.Client(), srv.URL, "k", "j1", false, ""); code == 0 {
		t.Fatal("a failed job must exit non-zero")
	}
}

func TestPrintRunResultSeparatesStatusFromRetrieval(t *testing.T) {
	var buf bytes.Buffer
	printRunResult(&buf, runJobEnvelope{
		Status: "complete",
		Retrieval: &retrievalView{
			Outcome: "blocked", Complete: false, FailureShape: "bot_wall",
			Reasons: []string{"challenge page served"}, RetryGuidance: "retry via a stealth tier",
		},
	})
	out := buf.String()
	for _, want := range []string{"complete", "not a complete answer", "bot_wall", "challenge page served", "retry via a stealth tier"} {
		if !strings.Contains(out, want) {
			t.Errorf("output should surface %q, got:\n%s", want, out)
		}
	}
}

func TestPrintRunResultReportsTruncation(t *testing.T) {
	var buf bytes.Buffer
	env := runJobEnvelope{Status: "complete"}
	env.Outputs = &runOutputs{Records: []map[string]any{{"title": "A"}}, Truncated: true}
	printRunResult(&buf, env)
	if !strings.Contains(buf.String(), "truncated") {
		t.Errorf("a truncated record set must say so, got:\n%s", buf.String())
	}
}

func TestSummarizeRecordIsDeterministic(t *testing.T) {
	if got := summarizeRecord(map[string]any{"title": "Shoe", "price": "$10"}); !strings.Contains(got, "Shoe") || !strings.Contains(got, "$10") {
		t.Errorf("summarizeRecord = %q", got)
	}
	// No preferred key: fall back to sorted keys so output does not vary per run.
	rec := map[string]any{"z": 1, "a": 2, "m": 3}
	first, second := summarizeRecord(rec), summarizeRecord(rec)
	if first != second {
		t.Errorf("summarizeRecord is non-deterministic: %q vs %q", first, second)
	}
	if !strings.HasPrefix(first, "a=") {
		t.Errorf("fallback should be sorted by key, got %q", first)
	}
}
