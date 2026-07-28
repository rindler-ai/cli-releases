package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The server sends its own explanation in {"error","class"} on every failure.
// The CLI used to replace it with a canned string keyed on the status code, so
// two very different 403s -- "this action needs a saved login" and "the site is
// not in your catalog" -- read identically, and both blamed the catalog.
//
// The canned text exists to add the FIX. It must not replace the diagnosis.
func TestTheServersOwnMessageSurvives(t *testing.T) {
	body := `{"error":"this action needs a saved login for example.com","class":"credential_required"}`
	err := runAuthError(http.StatusForbidden, body)
	if err == nil {
		t.Fatal("a 403 must be an error")
	}
	if !strings.Contains(err.Error(), "needs a saved login") {
		t.Errorf("the server's diagnosis was discarded: %q", err)
	}
}

func TestTheFixIsStillOffered(t *testing.T) {
	for _, tc := range []struct {
		code int
		want string
	}{
		{http.StatusUnauthorized, "rindler login"},
		{http.StatusNotFound, "rindler map"},
		{http.StatusTooManyRequests, "quota"},
	} {
		err := verbError("run", tc.code, `{"error":"nope"}`)
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("HTTP %d: %q does not tell the reader what to do (want %q)", tc.code, err, tc.want)
		}
	}
}

// An unparseable or empty body must not produce a message that is only
// punctuation; the status code is then the only thing we know, and we say so.
func TestAnEmptyBodyStillSaysSomething(t *testing.T) {
	for _, body := range []string{"", "not json", "{}", "<html>502</html>"} {
		err := verbError("sites", http.StatusBadGateway, body)
		if err == nil || len(strings.TrimSpace(err.Error())) < 12 {
			t.Errorf("body %q produced a useless error: %q", body, err)
		}
		if !strings.Contains(err.Error(), "502") {
			t.Errorf("body %q lost the status code: %q", body, err)
		}
	}
}

// Each command must speak for itself. `rindler sites` failing used to announce
// "run failed" and offer run's advice about a site the user never named.
func TestEachVerbNamesItself(t *testing.T) {
	if got := verbError("sites", http.StatusBadGateway, "").Error(); !strings.Contains(got, "sites") {
		t.Errorf("sites error says %q", got)
	}
	if got := verbError("device list", http.StatusBadGateway, "").Error(); !strings.Contains(got, "device list") {
		t.Errorf("device error says %q", got)
	}
	if got := verbError("sites", http.StatusBadGateway, "").Error(); strings.Contains(got, "run failed") {
		t.Errorf("sites borrowed run's vocabulary: %q", got)
	}
}

// runTerminal returns (false,false) for a job still in flight, so a marker
// keyed on ok alone stamped ✗ on every "queued" and "running".
func TestAnUnfinishedJobIsNotMarkedFailed(t *testing.T) {
	for _, status := range []string{"queued", "running", "pending"} {
		var b strings.Builder
		printRunResult(&b, runJobEnvelope{Status: status})
		if strings.Contains(b.String(), "✗") {
			t.Errorf("status %q rendered as a failure:\n%s", status, b.String())
		}
	}
	var done strings.Builder
	printRunResult(&done, runJobEnvelope{Status: "failed"})
	if !strings.Contains(done.String(), "✗") {
		t.Error("a genuinely failed job must still be marked failed")
	}
	var okRun strings.Builder
	printRunResult(&okRun, runJobEnvelope{Status: "complete"})
	if !strings.Contains(okRun.String(), "✓") {
		t.Error("a complete job must be marked succeeded")
	}
}

// A finished job becomes an exit code in ONE place, because it used to be two
// that disagreed: the follow path weighed `retrieval` and `run status --once` did
// not, so the same finished job exited 1 when followed and 0 when polled. A
// script using the shortcut read a bot-walled run as a win.
func TestOneVerdictForAFinishedRun(t *testing.T) {
	incomplete := &retrievalView{Outcome: "bot_wall", Complete: false}
	complete := &retrievalView{Outcome: "ok", Complete: true}

	for _, tc := range []struct {
		name string
		env  runJobEnvelope
		want int
	}{
		{"complete and retrieved", runJobEnvelope{Status: "complete", Retrieval: complete}, 0},
		// THE CASE THE SHORTCUT MISSED. The attempt ran; the site gave nothing.
		{"complete but retrieved nothing usable", runJobEnvelope{Status: "complete", Retrieval: incomplete}, 1},
		{"failed", runJobEnvelope{Status: "failed"}, 1},
		{"escalated", runJobEnvelope{Status: "needs_escalation"}, 1},
		{"expired", runJobEnvelope{Status: "expired"}, 1},
		// No retrieval reported at all: status is then the only verdict there is.
		{"complete, no retrieval reported", runJobEnvelope{Status: "complete"}, 0},
		// NOT finished is not a failure; the caller decides whether to wait.
		{"still running", runJobEnvelope{Status: "running", Retrieval: incomplete}, 0},
		{"queued", runJobEnvelope{Status: "queued"}, 0},
	} {
		if got := runExitCode(tc.env); got != tc.want {
			t.Errorf("%s: exit %d, want %d", tc.name, got, tc.want)
		}
	}
}

// A 404 on the JOB poll is an unknown job, not an unknown site. run's mapper
// answered it with "map it first: rindler map <url>", which sends someone to map
// a site that is mapped fine -- the job id is what is wrong.
func TestAnUnknownJobDoesNotBlameTheSite(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"job not found"}`))
	}))
	defer srv.Close()

	_, err := runJob(context.Background(), srv.Client(), srv.URL, "k", "no-such-job")
	if err == nil {
		t.Fatal("an unknown job must be an error")
	}
	if strings.Contains(err.Error(), "rindler map") {
		t.Errorf("blamed the site for an unknown job id: %q", err)
	}
	if !strings.Contains(err.Error(), "job") {
		t.Errorf("the message should name the job, got %q", err)
	}
}

// And a non-404 job-poll failure keeps the server's own words, like everywhere
// else, rather than falling back to run's site advice.
func TestAJobPollFailureUsesItsOwnVerb(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	_, err := runJob(context.Background(), srv.Client(), srv.URL, "k", "j1")
	if err == nil {
		t.Fatal("a 502 must be an error")
	}
	if !strings.Contains(err.Error(), "run status") {
		t.Errorf("the message should name the verb that failed, got %q", err)
	}
}

// "records: 5" is unreadable on its own: five of five and five of twelve hundred
// look identical, and the second is the one you needed to know about. The server
// sends the total and the CLI was dropping it.
func TestRecordCountShowsTheTotalWhenThereIsMore(t *testing.T) {
	// DECODED from JSON, not built in Go: the field has to arrive off the wire
	// for the json tag to be under test. Constructing it directly passes even
	// when the tag is wrong, which is how a decode bug hides behind a green test.
	var env runJobEnvelope
	if err := json.Unmarshal([]byte(
		`{"status":"complete","outputs":{"records":[{"a":1},{"a":2}],"total":1200}}`), &env); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	printRunResult(&b, env)
	if !strings.Contains(b.String(), "2 of 1200") {
		t.Errorf("the total is missing:\n%s", b.String())
	}

	// When the total matches what came back, the "of N" is noise.
	var b2 strings.Builder
	printRunResult(&b2, runJobEnvelope{Status: "complete", Outputs: &runOutputs{
		Records: []map[string]any{{"a": 1}}, Total: 1,
	}})
	if strings.Contains(b2.String(), " of ") {
		t.Errorf("a complete set should not be annotated:\n%s", b2.String())
	}

	// And a server that sends no total must not render "of 0".
	var b3 strings.Builder
	printRunResult(&b3, runJobEnvelope{Status: "complete", Outputs: &runOutputs{
		Records: []map[string]any{{"a": 1}},
	}})
	if strings.Contains(b3.String(), " of ") {
		t.Errorf("an absent total was rendered:\n%s", b3.String())
	}
}

// A truncated or walled result is exactly when someone wants the browser's own
// view. The server sends the link; the CLI was dropping it.
func TestTheRunViewerLinkIsSurfaced(t *testing.T) {
	var b strings.Builder
	printRunResult(&b, runJobEnvelope{
		Status:   "complete",
		Outputs:  &runOutputs{Records: []map[string]any{{"a": 1}}, Truncated: true},
		Evidence: &runEvidence{RunViewerURL: "https://app.example/runs/abc"},
	})
	out := b.String()
	if !strings.Contains(out, "https://app.example/runs/abc") {
		t.Errorf("the run-viewer link is missing:\n%s", out)
	}

	// No evidence, no line — never an empty "look at it:".
	var b2 strings.Builder
	printRunResult(&b2, runJobEnvelope{Status: "complete"})
	if strings.Contains(b2.String(), "look at it") {
		t.Errorf("printed a link line with no link:\n%s", b2.String())
	}
}

// A run that returned records and merely hit the list cap is a SUCCESS.
//
// The server marks such a run `complete: false` (records > 0 && truncated ->
// OutcomePartial), which is the ordinary case for any site with more rows than
// the cap — five by default. Treating incompleteness alone as failure made a
// perfectly healthy `rindler run` exit 1 and would have broken every script that
// checks the status. A false failure is worse than the silent success it replaced.
func TestATruncatedButHealthyRunSucceeds(t *testing.T) {
	env := runJobEnvelope{
		Status:    "complete",
		Outputs:   &runOutputs{Records: []map[string]any{{"a": 1}}, Truncated: true, Total: 1200},
		Retrieval: &retrievalView{Outcome: "partial", Complete: false},
	}
	if got := runExitCode(env); got != 0 {
		t.Fatalf("exit %d; a capped page is what the caller asked for", got)
	}
}

// Anything ELSE incomplete still fails. That is the case the split verdict exists
// for: a bot wall lets the job finish while returning nothing usable.
func TestANonTruncationPartialStillFails(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  runJobEnvelope
	}{
		{"bot wall with a reason", runJobEnvelope{
			Status:    "complete",
			Outputs:   &runOutputs{Records: []map[string]any{{"a": 1}}, Truncated: true},
			Retrieval: &retrievalView{Complete: false, Reasons: []string{"bot_wall"}},
		}},
		{"nothing came back at all", runJobEnvelope{
			Status:    "complete",
			Outputs:   &runOutputs{Records: nil, Truncated: true},
			Retrieval: &retrievalView{Complete: false},
		}},
		{"incomplete, not truncated", runJobEnvelope{
			Status:    "complete",
			Outputs:   &runOutputs{Records: []map[string]any{{"a": 1}}},
			Retrieval: &retrievalView{Complete: false},
		}},
		{"no outputs at all", runJobEnvelope{
			Status:    "complete",
			Retrieval: &retrievalView{Complete: false},
		}},
	} {
		if got := runExitCode(tc.env); got != 1 {
			t.Errorf("%s: exit %d, want 1", tc.name, got)
		}
	}
}
