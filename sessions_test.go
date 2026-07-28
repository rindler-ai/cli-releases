package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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
			[]string{"search"}, nil, "", 0, tc.session, tc.session == "")
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

// CONCURRENT BINDS. Every `rindler run` is its own process, so two runs are two
// writers with no in-process lock available between them.
//
// This started at 0 of 20 surviving. The cause was a SHARED temp path: writers
// interleaved, one renamed another's half-written file into place, and the book
// that landed was unreadable — which loadSessions reads as empty, losing every
// name at once rather than one of them.
func TestConcurrentBindsKeepEveryName(t *testing.T) {
	isolate(t)
	const n = 20
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			_ = bindSession(fmt.Sprintf("name%02d", k), fmt.Sprintf("sess-%02d", k))
		}(i)
	}
	wg.Wait()

	book := loadSessions()
	if book.Bound == nil {
		t.Fatal("the book decoded to nil, which panics on the next write")
	}
	if len(book.Bound) != n {
		t.Fatalf("kept %d of %d names; concurrent writers are losing entries", len(book.Bound), n)
	}
	// Every entry must be its OWN id, not another writer's.
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("name%02d", i)
		if got := book.Bound[name]; got != fmt.Sprintf("sess-%02d", i) {
			t.Errorf("%s bound to %q", name, got)
		}
	}
}

// A STALE lock must cost one slow bind, not a wedged CLI. A killed process leaves
// the lock file behind, and refusing to run then would trade a convenience for a
// broken command.
func TestAStaleLockDoesNotWedgeTheCLI(t *testing.T) {
	isolate(t)
	p, err := sessionsPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	// Nobody is coming back for this one.
	if err := os.WriteFile(p+".lock", nil, 0o600); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	if err := bindSession("stubborn", "sess-1"); err != nil {
		t.Fatalf("a stale lock must not fail the bind: %v", err)
	}
	if got := sessionIDFor("stubborn"); got != "sess-1" {
		t.Fatalf("the bind did not land: %q", got)
	}
	// It should have waited, then proceeded — not waited forever.
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("waited %s on a stale lock", elapsed)
	}
}

// The lock must not be left behind on a normal bind, or the NEXT bind pays the
// full stale-lock wait for no reason.
func TestTheLockIsReleased(t *testing.T) {
	isolate(t)
	if err := bindSession("a", "sess-a"); err != nil {
		t.Fatal(err)
	}
	p, _ := sessionsPath()
	if _, err := os.Stat(p + ".lock"); !os.IsNotExist(err) {
		t.Fatal("the lock file survived a successful bind")
	}
	// And no stray temp files, which would accumulate one per run forever.
	entries, err := os.ReadDir(filepath.Dir(p))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Errorf("left a temp file behind: %s", e.Name())
		}
	}
}

// The unique temp path guards the case where the LOCK IS BYPASSED.
//
// With the lock held, writers serialise and a shared temp name would be harmless.
// But the lock is best-effort: a stale one is waited out and then ignored, at
// which point concurrent writers are back. On a shared temp path they interleave
// and one publishes another's half-written file, which loadSessions reads as an
// EMPTY book — losing every name rather than one.
//
// So: hold the lock stale, force every writer past it, and require that the book
// is still a readable book afterwards.
func TestAByPassedLockStillCannotCorruptTheBook(t *testing.T) {
	isolate(t)
	p, err := sessionsPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	// Seed a real book, so "corrupted to empty" is distinguishable from "nothing
	// was ever written".
	if err := os.WriteFile(p, []byte(`{"version":1,"bound":{"seed":"sess-seed"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// A lock nobody will release: every writer waits it out, then proceeds.
	if err := os.WriteFile(p+".lock", nil, 0o600); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			_ = bindSession(fmt.Sprintf("n%d", k), fmt.Sprintf("s%d", k))
		}(i)
	}
	wg.Wait()

	book := loadSessions()
	if book.Bound == nil {
		t.Fatal("the book decoded to nil after concurrent lock-bypassed writes")
	}
	// Last-writer-wins may drop some additions; that is documented and
	// self-healing. Losing EVERYTHING is the failure this guards.
	if len(book.Bound) == 0 {
		t.Fatal("every name was lost: a writer published a half-written book")
	}
}

// THE TEST THAT WAS MISSING, and whose absence let the entire named-session
// feature ship inert.
//
// Two independent holes, both invisible without an end-to-end binding test:
//
//  1. the first run CLOSED the session it opened, so there was never a live one
//     for a name to point at. Fixed by keep_session.
//  2. the session id was read from a poll fired immediately after the 202, but
//     the server writes it when the run STARTS its browser. That poll always saw
//     nothing, so the name never bound.
//
// Net effect: `--session trip` twice gave two unrelated browsers and said nothing
// was wrong. Every unit test passed, because none of them asserted that a NAME
// ends up bound to an ID.
func TestANamedRunActuallyBindsTheSessionItGot(t *testing.T) {
	isolate(t)
	t.Setenv("RINDLER_API_KEY", "rindler_live_test")

	var sawKeep bool
	polls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/runtime/run", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		sawKeep, _ = body["keep_session"].(bool)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"job_id":"j1"}`))
	})
	mux.HandleFunc("GET /v1/runtime/jobs/{id}", func(w http.ResponseWriter, _ *http.Request) {
		polls++
		// The REAL ordering: no session on the first poll, because the browser has
		// not started yet. A test that returned one immediately would pass against
		// the broken code.
		if polls == 1 {
			_, _ = w.Write([]byte(`{"job_id":"j1","status":"running"}`))
			return
		}
		_, _ = w.Write([]byte(`{"job_id":"j1","status":"complete","session_id":"sess-real",
		                        "outputs":{"records":[]},"retrieval":{"outcome":"ok","complete":true}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	if code := runRun([]string{
		"--site", "example.com", "--action", "search",
		"--session", "trip", "--api-base", srv.URL,
	}); code != 0 {
		t.Fatalf("run exited %d", code)
	}

	if !sawKeep {
		t.Error("the first run must ask the server to KEEP the session, or there is nothing to reuse")
	}
	if got := sessionIDFor("trip"); got != "sess-real" {
		t.Fatalf("the name bound to %q, want sess-real -- named sessions are inert", got)
	}
}

// The SECOND run must send the bound id and must NOT ask to keep: whoever opened
// the session owns its lifetime.
func TestASecondNamedRunReusesAndDoesNotAskToKeep(t *testing.T) {
	isolate(t)
	t.Setenv("RINDLER_API_KEY", "rindler_live_test")
	if err := bindSession("trip", "sess-existing"); err != nil {
		t.Fatal(err)
	}

	var sentID string
	var sawKeep bool
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/runtime/run", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		sentID, _ = body["session_id"].(string)
		sawKeep, _ = body["keep_session"].(bool)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"job_id":"j2"}`))
	})
	mux.HandleFunc("GET /v1/runtime/jobs/{id}", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"job_id":"j2","status":"complete","session_id":"sess-existing",
		                        "outputs":{"records":[]},"retrieval":{"outcome":"ok","complete":true}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	if code := runRun([]string{
		"--site", "example.com", "--action", "search",
		"--session", "trip", "--api-base", srv.URL,
	}); code != 0 {
		t.Fatalf("run exited %d", code)
	}
	if sentID != "sess-existing" {
		t.Errorf("sent session_id %q, want the bound one", sentID)
	}
	if sawKeep {
		t.Error("a reusing run must not ask to keep somebody else's session alive")
	}
}
