package main

// `rindler run <site> "<task>"` is the verb this CLI exists for, so the two
// things it must never get wrong are pinned here:
//
//   1. WHICH SHAPE IT DISPATCHES TO. Two positionals is the task verb; the
//      structured form and `run status` must still reach their own handlers.
//   2. WHICH FAILURE IT REPORTS. "this site cannot" and "we failed, retry" are
//      different answers, and collapsing them tells a customer their site
//      cannot do something it can.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExitCodeDistinguishesTheOutcomes(t *testing.T) {
	for _, tc := range []struct {
		outcome string
		want    int
		why     string
	}{
		{"built", 0, "the only success"},
		{"cannot", 3, "the SITE cannot -- retrying will never help"},
		{"nearly", 4, "needs a confirmation the CLI cannot collect"},
		{"unavailable", 1, "WE failed -- a retry is reasonable"},
		{"blocked", 1, "not attempted"},
		{"something-a-newer-server-invents", 1, "unknown is never a success"},
	} {
		if got := exitForAnswer(taskResponse{Outcome: tc.outcome}); got != tc.want {
			t.Errorf("%s: exit %d, want %d (%s)", tc.outcome, got, tc.want, tc.why)
		}
	}
}

// A body this version cannot read is OUR failure and must not be printed at the
// customer: an HTML error page or a proxy notice is not an answer about their
// site.
func TestAnUnreadableBodyIsNotShownToTheCustomer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html><body>502 Bad Gateway</body></html>"))
	}))
	defer srv.Close()
	t.Setenv("RINDLER_AUTHORIZE_BASE", srv.URL)

	_, _, err := postTask(t.Context(), "rindler_live_x", taskRequest{Site: "a.com", Task: "do it"})
	if err == nil {
		t.Fatal("an unparseable body must be an error, not an answer")
	}
	if strings.Contains(err.Error(), "<html>") || strings.Contains(err.Error(), "Bad Gateway</body>") {
		t.Errorf("the raw upstream body reached the customer: %v", err)
	}
}

// The OUTCOME is the discriminator, never the HTTP status, so a proxy that
// rewrites a status cannot change what the customer is told.
func TestTheOutcomeFieldDecidesNotTheHttpStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// A deliberately mismatched pair: 200 carrying a refusal.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"outcome": "cannot",
			"message": "chase.com cannot book flights",
		})
	}))
	defer srv.Close()
	t.Setenv("RINDLER_AUTHORIZE_BASE", srv.URL)

	answer, _, err := postTask(t.Context(), "rindler_live_x", taskRequest{Site: "chase.com", Task: "book a flight"})
	if err != nil {
		t.Fatalf("postTask: %v", err)
	}
	if got := exitForAnswer(answer); got != 3 {
		t.Errorf("a 200 carrying `cannot` must still exit 3, got %d", got)
	}
}

// The key the customer holds, and nothing else, authenticates the call.
func TestTheCallersOwnKeyIsSent(t *testing.T) {
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"outcome":"built","name":"n","summary":"s"}`))
	}))
	defer srv.Close()
	t.Setenv("RINDLER_AUTHORIZE_BASE", srv.URL)

	if _, _, err := postTask(t.Context(), "rindler_live_mine", taskRequest{Site: "a.com", Task: "t"}); err != nil {
		t.Fatalf("postTask: %v", err)
	}
	if seen != "Bearer rindler_live_mine" {
		t.Errorf("Authorization = %q, want the caller's own key", seen)
	}
}

// The site and the sentence both reach the server. A request that dropped the
// sentence would build whatever the site's first action happens to be.
func TestBothTheSiteAndTheSentenceAreSent(t *testing.T) {
	var body taskRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"outcome":"built","name":"n"}`))
	}))
	defer srv.Close()
	t.Setenv("RINDLER_AUTHORIZE_BASE", srv.URL)

	if _, _, err := postTask(t.Context(), "k", taskRequest{Site: "chase.com", Task: "download statements"}); err != nil {
		t.Fatalf("postTask: %v", err)
	}
	if body.Site != "chase.com" || body.Task != "download statements" {
		t.Errorf("server saw %+v", body)
	}
}

// No user-facing line may name a config, an action id, a chain or an MCP
// server. That vocabulary is the thing the product pivot is removing, and it
// leaks through error copy long after the help text is clean.
func TestNoInternalVocabularyReachesTheCustomer(t *testing.T) {
	banned := []string{"MCP", "mcp server", "action id", "chain", "config", "selector"}
	for _, outcome := range []string{"built", "nearly", "cannot", "unauthorized", "blocked", "unavailable"} {
		var b strings.Builder
		printTaskAnswerTo(&b, &b, taskResponse{Outcome: outcome, Name: "n", Summary: "s"})
		for _, word := range banned {
			if strings.Contains(strings.ToLower(b.String()), strings.ToLower(word)) {
				t.Errorf("outcome %q leaks %q to the customer:\n%s", outcome, word, b.String())
			}
		}
	}
}

// DISPATCH. `run` carries three shapes and picking the wrong one is silent:
// the structured form would be read as a two-word sentence, and `run status`
// would be read as a site called "status".
func TestRunDispatchPicksTheRightShape(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string // "task" | "structured" | "status"
	}{
		{"two positionals is the task verb", []string{"chase.com", "download my statements"}, "task"},
		{"a sentence with flags after it is still the task verb",
			[]string{"chase.com", "download my statements", "--json"}, "task"},
		{"the structured form still reaches its own handler",
			[]string{"--site", "chase.com", "--action", "list_accounts"}, "structured"},
		{"run status is not a site called status", []string{"status", "job-123"}, "status"},
		{"a bare site with no sentence is not the task verb", []string{"chase.com"}, "structured"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := runShapeFor(tc.args); got != tc.want {
				t.Errorf("runShapeFor(%q) = %s, want %s", tc.args, got, tc.want)
			}
		})
	}
}

// A BUILT AUTOMATION WHOSE RUN FAILED IS NOT A SUCCESS. Found live on
// 2026-07-31: a task on allbirds.com built fine, its one step failed with
// page_loading, and the outcome-only mapping exited 0 -- telling a script the
// thing happened when it had not.
func TestABuiltAutomationWhoseRunFailedDoesNotExitZero(t *testing.T) {
	failed := taskResponse{Outcome: "built", Name: "n"}
	failed.Schedule = &struct {
		State  string `json:"state"`
		Reason string `json:"reason,omitempty"`
	}{State: "not_armed", Reason: "step 1 (search_products) failed (page_loading)"}

	if got := exitForAnswer(failed); got == 0 {
		t.Fatal("a saved automation whose run failed must not exit 0")
	}
	if got := exitForAnswer(failed); got != 5 {
		t.Errorf("exit %d, want 5 (saved, but the run did not succeed)", got)
	}

	// ...and the customer must see it in the FIRST line, not the second.
	var b strings.Builder
	printTaskAnswerTo(&b, &b, failed)
	if strings.HasPrefix(b.String(), "\u2713") {
		t.Errorf("a failed run claimed the success mark:\n%s", b.String())
	}
	if !strings.Contains(b.String(), "page_loading") {
		t.Errorf("the reason the run failed was dropped:\n%s", b.String())
	}
}

// The ordinary success still exits 0 and still claims the tick.
func TestARunThatWorkedExitsZero(t *testing.T) {
	ok := taskResponse{Outcome: "built", Name: "n", Summary: "s"}
	ok.Schedule = &struct {
		State  string `json:"state"`
		Reason string `json:"reason,omitempty"`
	}{State: "unscheduled"}

	if got := exitForAnswer(ok); got != 0 {
		t.Errorf("a run that worked exited %d, want 0", got)
	}
	var b strings.Builder
	printTaskAnswerTo(&b, &b, ok)
	if !strings.HasPrefix(b.String(), "\u2713") {
		t.Errorf("a successful run did not claim the tick:\n%s", b.String())
	}
}
