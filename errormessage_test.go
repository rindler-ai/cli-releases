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
