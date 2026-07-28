package main

import (
	"net/http"
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
