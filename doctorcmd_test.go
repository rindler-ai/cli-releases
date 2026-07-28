package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func findCheck(checks []check, name string) (check, bool) {
	for _, c := range checks {
		if c.Name == name {
			return c, true
		}
	}
	return check{}, false
}

func TestDiagnoseFlagsMissingLoginAsFailure(t *testing.T) {
	t.Setenv("RINDLER_CONFIG_DIR", t.TempDir())
	got, ok := findCheck(diagnose(cliConfig{}, "", false, false), "login")
	if !ok || got.State != checkFail {
		t.Fatalf("logged out must FAIL, got %+v", got)
	}
	if !strings.Contains(got.Fix, "rindler login") {
		t.Errorf("fix should name the command, got %q", got.Fix)
	}
}

func TestDiagnoseAcceptsEnvKey(t *testing.T) {
	t.Setenv("RINDLER_CONFIG_DIR", t.TempDir())
	got, ok := findCheck(diagnose(cliConfig{}, "", true, false), "login")
	if !ok || got.State != checkOK {
		t.Fatalf("an env key must satisfy the login check, got %+v", got)
	}
	// Expiry and mapper checks are meaningless for an env key; they must not be
	// emitted as warnings the user cannot act on.
	if _, ok := findCheck(diagnose(cliConfig{ExpiresAt: "2020-01-01T00:00:00Z"}, "", true, false), "key expiry"); ok {
		t.Error("env-key mode should not report a stored-key expiry")
	}
}

// expires_at is a snapshot taken when the key was MINTED, and the server can
// have invalidated it in either direction since: a revoked key still looks
// valid here, and a refreshed session still looks dead. So which check owns the
// verdict depends on whether the live probe is going to run.
//
// With the live leg, this is a WARNING that explains the failure the probe is
// about to report. It used to be the hard failure itself, which sent people to
// `rindler login` over a stale local file while their key worked fine.
func TestExpiredKeyWarnsWhenTheLiveProbeWillRule(t *testing.T) {
	t.Setenv("RINDLER_CONFIG_DIR", t.TempDir())
	past := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	got, ok := findCheck(diagnose(cliConfig{ExpiresAt: past}, "", false, false), "key expiry")
	if !ok {
		t.Fatal("an expired key must still be reported")
	}
	if got.State != checkWarn {
		t.Fatalf("want a warning deferring to the live probe, got %+v", got)
	}
	if got.Fix == "" {
		t.Error("the warning must still name the fix")
	}
}

// --offline removes the only authority there is, so the snapshot has to carry
// the verdict: reporting a stale key as merely a warning with nothing to
// override it would let doctor exit clean on a login that cannot work.
func TestExpiredKeyFailsWhenThereIsNoLiveProbe(t *testing.T) {
	t.Setenv("RINDLER_CONFIG_DIR", t.TempDir())
	past := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	got, ok := findCheck(diagnose(cliConfig{ExpiresAt: past}, "", false, true), "key expiry")
	if !ok || got.State != checkFail {
		t.Fatalf("offline, an expired key must FAIL, got %+v", got)
	}
}

// The live probe is the authority, and it already fails on a rejected key.
func TestTheLiveProbeIsTheAuthorityOnAnUnacceptedKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	got := pingAPI(context.Background(), srv.Client(), srv.URL, "rindler_live_test")
	if got.State != checkFail {
		t.Fatalf("a 401 from the server must FAIL, got %+v", got)
	}
}

func TestDiagnoseFutureKeyIsOK(t *testing.T) {
	t.Setenv("RINDLER_CONFIG_DIR", t.TempDir())
	// Beyond the 3-day "expires soon" band in cliConfig.expiryStatus.
	future := time.Now().Add(10 * 24 * time.Hour).UTC().Format(time.RFC3339)
	got, ok := findCheck(diagnose(cliConfig{ExpiresAt: future}, "", false, false), "key expiry")
	if !ok || got.State != checkOK {
		t.Fatalf("a comfortably valid key must pass, got %+v", got)
	}
}

// The middle band matters on its own: a key expiring within 3 days is still
// usable, so it must WARN rather than fail, or doctor would report a working
// setup as broken.
func TestDiagnoseExpiringSoonIsWarnNotFail(t *testing.T) {
	t.Setenv("RINDLER_CONFIG_DIR", t.TempDir())
	soon := time.Now().Add(12 * time.Hour).UTC().Format(time.RFC3339)
	got, ok := findCheck(diagnose(cliConfig{ExpiresAt: soon}, "", false, false), "key expiry")
	if !ok || got.State != checkWarn {
		t.Fatalf("a key expiring soon must WARN, got %+v", got)
	}
}

// Mapping is a WARNING, never a failure: every verb except `map` works without
// it, so failing here would tell a healthy setup it is broken.
func TestDiagnoseMappingAbsentIsWarnNotFail(t *testing.T) {
	t.Setenv("RINDLER_CONFIG_DIR", t.TempDir())
	got, ok := findCheck(diagnose(cliConfig{}, "", false, false), "site mapping")
	if !ok || got.State != checkWarn {
		t.Fatalf("absent mapping must WARN, got %+v", got)
	}
	if got, _ := findCheck(diagnose(cliConfig{MapperAccess: true}, "", false, false), "site mapping"); got.State != checkOK {
		t.Errorf("granted mapping must pass, got %+v", got)
	}
}

func TestDiagnoseWarnsOnNonDefaultAPIOrigin(t *testing.T) {
	t.Setenv("RINDLER_CONFIG_DIR", t.TempDir())
	got, _ := findCheck(diagnose(cliConfig{}, "https://preview.example", false, false), "api origin")
	if got.State != checkWarn || !strings.Contains(got.Detail, "not the default") {
		t.Errorf("a non-default origin should warn, got %+v", got)
	}
	if got, _ := findCheck(diagnose(cliConfig{}, defaultAPIBase, false, false), "api origin"); got.State != checkOK {
		t.Errorf("the default origin should pass, got %+v", got)
	}
}

func TestPingAPIClassifies(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"configs":[{"domain":"a.com"},{"domain":"b.com"}]}`))
	}))
	defer ok.Close()
	if c := pingAPI(context.Background(), ok.Client(), ok.URL, "k"); c.State != checkOK || !strings.Contains(c.Detail, "2 site") {
		t.Errorf("healthy ping = %+v", c)
	}

	un := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer un.Close()
	c := pingAPI(context.Background(), un.Client(), un.URL, "k")
	if c.State != checkFail || !strings.Contains(c.Fix, "rindler login") {
		t.Errorf("401 ping should fail and point at login, got %+v", c)
	}

	boom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer boom.Close()
	if c := pingAPI(context.Background(), boom.Client(), boom.URL, "k"); c.State != checkFail {
		t.Errorf("500 ping should fail, got %+v", c)
	}
}

func TestCheckMarks(t *testing.T) {
	if (check{State: checkOK}).mark() != "✓" ||
		(check{State: checkWarn}).mark() != "!" ||
		(check{State: checkFail}).mark() != "✗" {
		t.Error("check marks should be distinguishable")
	}
}

// doctor must diagnose the origin the COMMANDS use. A fourth hand-rolled copy
// of the ladder skipped RINDLER_API_BASE while its own Fix line told the reader
// to unset that variable — advice that could not have been the cause, because
// nothing there read it.
func TestDoctorDiagnosesTheOriginTheCommandsActuallyUse(t *testing.T) {
	t.Setenv("RINDLER_CONFIG_DIR", t.TempDir())
	t.Setenv("RINDLER_API_BASE", "https://staging.example")
	got, ok := findCheck(diagnose(cliConfig{}, "", false, true), "api origin")
	if !ok {
		t.Fatal("api origin must be reported")
	}
	if !strings.Contains(got.Detail, "staging.example") {
		t.Fatalf("doctor reports %q while the commands use https://staging.example", got.Detail)
	}
}
