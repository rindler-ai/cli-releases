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
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
	// Ran:true is the point -- this is a walk that FIRED and failed, not a
	// draft waiting on an input. The two share the not_armed schedule.
	failed := taskResponse{Outcome: "built", Name: "n", Ran: true}
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

// NEEDS AN ANSWER IS NOT A FAILED RUN. Both arrive as built + not_armed;
// only `ran` tells them apart. Measured on the dev deploy: ahs.com and
// portal.vtcourts.gov returned in 4 seconds, far too fast for a browser,
// because they were asking for an input.
func TestAnAutomationWaitingOnAnInputIsNotReportedAsAFailedRun(t *testing.T) {
	waiting := taskResponse{Outcome: "built", Name: "n", Ran: false}
	waiting.Schedule = &struct {
		State  string `json:"state"`
		Reason string `json:"reason,omitempty"`
	}{State: "not_armed", Reason: "Enter a ZIP code, then start quote."}

	if got := exitForAnswer(waiting); got != 4 {
		t.Errorf("exit %d, want 4 (needs one thing answered first)", got)
	}

	ranAndFailed := waiting
	ranAndFailed.Ran = true
	ranAndFailed.Schedule = waiting.Schedule
	if got := exitForAnswer(ranAndFailed); got != 5 {
		t.Errorf("a run that fired and failed exited %d, want 5", got)
	}

	// And the marks differ, so the first line already says which it is.
	var a, b strings.Builder
	printTaskAnswerTo(&a, &a, waiting)
	printTaskAnswerTo(&b, &b, ranAndFailed)
	if !strings.HasPrefix(a.String(), "?") {
		t.Errorf("waiting-on-input did not get the question mark:\n%s", a.String())
	}
	if !strings.HasPrefix(b.String(), "!") {
		t.Errorf("ran-and-failed did not get the warning mark:\n%s", b.String())
	}
}

// THE CLIENT MUST NOT UNDERCUT THE CONTEXT.
//
// defaultHTTPClient() caps every request at 30 seconds, which is right for the
// other verbs and fatal here: a first pass drives a real browser. Measured on
// the dev deploy, a build took 25s and a slower one died at exactly 30s while
// the server was still working.
//
// A FIRST VERSION OF THIS TEST WAS VACUOUS and is worth remembering. It drove
// postTask with a 150ms context against a hanging server and asserted the
// context ended the call -- which is true whether the client caps at 30s or
// not, because 150ms comes first either way. Reverting the fix left it green.
// The property is about the CLIENT, so the client is what it inspects.
func TestTheTaskClientImposesNoCeilingOfItsOwn(t *testing.T) {
	if got := taskHTTPClient().Timeout; got != 0 {
		t.Errorf("task client Timeout = %v, want 0 so the context owns the budget", got)
	}
	// Stated as a CONTRAST, so this fails loudly if someone "helpfully" makes
	// the shared client unlimited instead -- which would silently remove the
	// 30s ceiling every other verb relies on.
	if got := defaultHTTPClient().Timeout; got != 30*time.Second {
		t.Errorf("defaultHTTPClient Timeout = %v, want 30s; the contrast is the point", got)
	}
}

// The context still ends a call that outlives it, and says so as a slow RUN
// rather than a broken server.
func TestASlowRunIsReportedAsSlowNotBroken(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		_, _ = w.Write([]byte(`{"outcome":"built","name":"n"}`))
	}))
	defer srv.Close()
	t.Setenv("RINDLER_AUTHORIZE_BASE", srv.URL)

	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()
	_, _, err := postTask(ctx, "k", taskRequest{Site: "a.com", Task: "t"})
	close(release)

	if err == nil {
		t.Fatal("expected the context to end the call")
	}
	if !strings.Contains(err.Error(), "still going") {
		t.Errorf("a deadline should be reported as a slow RUN, got: %v", err)
	}
}
