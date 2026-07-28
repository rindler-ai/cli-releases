package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Named sessions live ENTIRELY on this machine: the server only ever sees an id.
// These tests pin the two things that make that work -- the name map surviving
// honestly, and a dead id re-binding rather than erroring.

func TestABoundNameResolvesBackToItsID(t *testing.T) {
	isolate(t)
	if got := sessionIDFor("mywork"); got != "" {
		t.Fatalf("a fresh machine returned %q", got)
	}
	if err := bindSession("mywork", "sess-1"); err != nil {
		t.Fatal(err)
	}
	if got := sessionIDFor("mywork"); got != "sess-1" {
		t.Fatalf("sessionIDFor = %q, want sess-1", got)
	}
	// Case-insensitive: a name is typed by a human at a shell, and "Mywork"
	// missing "mywork" would silently open a second browser with no error.
	for _, variant := range []string{"MyWork", "  mywork  ", "MYWORK"} {
		if got := sessionIDFor(variant); got != "sess-1" {
			t.Errorf("sessionIDFor(%q) = %q, want sess-1", variant, got)
		}
	}
}

// Re-binding is how re-attach works, so it must overwrite rather than refuse.
func TestRebindingAnameReplacesTheID(t *testing.T) {
	isolate(t)
	_ = bindSession("trip", "sess-old")
	if err := bindSession("trip", "sess-new"); err != nil {
		t.Fatal(err)
	}
	if got := sessionIDFor("trip"); got != "sess-new" {
		t.Fatalf("re-bind left %q; a name must be able to follow a fresh session", got)
	}
}

// Lowest UNUSED, not highest-plus-one: killing 0 must make "0" available again
// rather than counting away from zero forever.
func TestAutoNamingReusesTheLowestFreeNumber(t *testing.T) {
	book := sessionBook{Bound: map[string]string{"0": "a", "1": "b", "3": "d"}}
	if got := nextAutoName(book); got != "2" {
		t.Fatalf("nextAutoName = %q, want the lowest free number 2", got)
	}
	if got := nextAutoName(sessionBook{Bound: map[string]string{}}); got != "0" {
		t.Fatalf("first session = %q, want 0", got)
	}
	// A named session must not consume a NUMBER.
	if got := nextAutoName(sessionBook{Bound: map[string]string{"trip": "x"}}); got != "0" {
		t.Fatalf("a named session consumed a number: %q", got)
	}
}

// A corrupt or missing book is an EMPTY book, never an error. A lost name map
// costs an extra browser; refusing to run because a convenience file is
// unreadable would be the worse trade.
func TestACorruptBookDoesNotBreakTheCLI(t *testing.T) {
	isolate(t)
	p, err := sessionsPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, junk := range []string{"", "not json", `{"bound":null}`, `[]`} {
		if err := os.WriteFile(p, []byte(junk), 0o600); err != nil {
			t.Fatal(err)
		}
		book := loadSessions()
		if book.Bound == nil {
			t.Fatalf("junk %q produced a nil map, which panics on write", junk)
		}
		if got := sessionIDFor("anything"); got != "" {
			t.Errorf("junk %q resolved a name to %q", junk, got)
		}
	}
}

// The book holds no secrets, but it names live sessions, so it should not be
// world-readable.
func TestTheBookIsNotWorldReadable(t *testing.T) {
	isolate(t)
	if err := bindSession("x", "sess-1"); err != nil {
		t.Fatal(err)
	}
	p, _ := sessionsPath()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("sessions.json mode is %04o, want 0600", fi.Mode().Perm())
	}
}

// RE-ATTACH. Only "that session is gone" may be retried, and only once.
func TestOnlyAGoneSessionIsRetryable(t *testing.T) {
	for _, tc := range []struct {
		msg  string
		want bool
	}{
		{"session_not_found", true},
		{"that session is not available; omit session_id to start a fresh one", true},
		{"SESSION_NOT_FOUND", true},
		// Everything else is a real failure, and retrying it would just do the
		// same thing twice.
		{"rate limited or out of quota", false},
		{"not logged in or the key expired", false},
		{"no config for that site", false},
		{"", false},
	} {
		var err error
		if tc.msg != "" {
			err = &stubErr{tc.msg}
		}
		if got := isSessionGone(err); got != tc.want {
			t.Errorf("isSessionGone(%q) = %v, want %v", tc.msg, got, tc.want)
		}
	}
}

type stubErr struct{ s string }

func (e *stubErr) Error() string { return e.s }

// The wire contract: session_id is sent ONLY when reusing. An empty one is a
// request to reuse nothing, which the server refuses rather than reading as
// "open a fresh session".
func TestSessionIDIsSentOnlyWhenReusing(t *testing.T) {
	for _, tc := range []struct {
		session  string
		wantSent bool
	}{
		{"", false},
		{"sess-1", true},
	} {
		var got map[string]any
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewDecoder(r.Body).Decode(&got)
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"job_id":"j1"}`))
		}))
		_, err := startRunWithSession(t.Context(), srv.Client(), srv.URL, "k", "example.com",
			[]string{"search"}, nil, "", 0, tc.session)
		srv.Close()
		if err != nil {
			t.Fatalf("session %q: %v", tc.session, err)
		}
		_, present := got["session_id"]
		if present != tc.wantSent {
			t.Errorf("session %q: sent=%v want %v", tc.session, present, tc.wantSent)
		}
	}
}

// `kill` must drop the binding even when the server call fails. The caller asked
// to be rid of the name; leaving it bound to a session they believe is gone is
// the worse outcome, and the idle reaper is the backstop for the browser.
func TestKillDropsTheBindingEvenIfTheServerFails(t *testing.T) {
	isolate(t)
	t.Setenv("RINDLER_API_KEY", "rindler_live_test")
	t.Setenv("RINDLER_API_BASE", "https://nonexistent.invalid")
	_ = bindSession("doomed", "sess-1")

	if code := runKill([]string{"doomed"}); code != 0 {
		t.Fatalf("kill exited %d; a failed remote close must not fail the local drop", code)
	}
	if got := sessionIDFor("doomed"); got != "" {
		t.Fatalf("the binding survived as %q", got)
	}
}

func TestKillRefusesAnUnknownName(t *testing.T) {
	isolate(t)
	if code := runKill([]string{"never-existed"}); code == 0 {
		t.Fatal("killing an unknown name should not report success")
	}
	if code := runKill(nil); code != 2 {
		t.Error("no argument should be a usage error")
	}
}

func TestSessionsListsAndSortsNumerically(t *testing.T) {
	isolate(t)
	for name, id := range map[string]string{"10": "a", "2": "b", "trip": "c"} {
		_ = bindSession(name, id)
	}
	got := sortedSessionNames(loadSessions())
	want := []string{"2", "10", "trip"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v, want %v (2 before 10, names last)", got, want)
	}
	if code := runSessions(nil); code != 0 {
		t.Errorf("sessions exited %d", code)
	}
	if code := runSessions([]string{"--json"}); code != 0 {
		t.Errorf("sessions --json exited %d", code)
	}
}
