package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Named sessions, held ENTIRELY on this machine.
//
// A session's real identity is the server's id. A name is a local convenience,
// so it lives here and never goes over the wire. That is the whole reason this
// feature is a small file instead of a server subsystem: a server-side name
// table would have to answer who may see a name, whether two members may reuse
// one, and what happens to a name whose session died -- three tenancy questions
// bought in exchange for an affordance a JSON file provides.
//
// It also gives transparent re-attach for free. When the server no longer knows
// a bound id, the CLI opens a fresh session and rebinds the SAME NAME. The
// browser really did die; the name is what survived, which is the honest version
// of "the session persists". A server that resurrected sessions would be both
// harder and a lie, because the page state is gone either way.

const sessionsFileName = "sessions.json"

// sessionBook is the on-disk name -> session id map.
type sessionBook struct {
	Version int               `json:"version"`
	Bound   map[string]string `json:"bound"`
}

func sessionsPath() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, sessionsFileName), nil
}

// loadSessions reads the book. A missing or unreadable file is an EMPTY book,
// not an error: a lost name map costs an extra browser, and refusing to run
// because a convenience file is corrupt would be a worse trade.
func loadSessions() sessionBook {
	empty := sessionBook{Version: 1, Bound: map[string]string{}}
	p, err := sessionsPath()
	if err != nil {
		return empty
	}
	b, err := os.ReadFile(p)
	if err != nil || len(b) == 0 {
		return empty
	}
	var book sessionBook
	if json.Unmarshal(b, &book) != nil || book.Bound == nil {
		return empty
	}
	return book
}

// withSessionBookLock serialises a read-modify-write of the book across
// PROCESSES. Every `rindler run` is its own process, so a mutex would buy
// nothing: two concurrent runs are two concurrent writers.
//
// O_CREATE|O_EXCL is the lock, because it is the one atomic
// create-if-absent the standard library offers on both platforms this ships to.
// No flock: that is Unix-only, and this repo is stdlib-only by policy.
//
// BEST EFFORT BY DESIGN. If the lock cannot be taken within the window it
// proceeds anyway, which degrades to last-writer-wins -- the behaviour before
// this existed. That is deliberate: a name is a convenience, and refusing to run
// because a lock file is held (or was left behind by a killed process) would
// trade a small inconvenience for a broken command.
//
// A stale lock therefore costs one slow bind, not a wedged CLI.
func withSessionBookLock(fn func() error) error {
	p, err := sessionsPath()
	if err != nil {
		return fn()
	}
	lock := p + ".lock"
	if err := os.MkdirAll(filepath.Dir(lock), 0o700); err != nil {
		return fn()
	}
	deadline := time.Now().Add(sessionLockWait)
	for {
		f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_ = f.Close()
			defer os.Remove(lock)
			return fn()
		}
		if time.Now().After(deadline) {
			// Give up on the lock, not on the work.
			return fn()
		}
		time.Sleep(sessionLockPoll)
	}
}

// sessionLockWait is short on purpose: the critical section is one small file
// read and write, so anything slower than this is a stale lock rather than
// contention worth waiting out.
const (
	sessionLockWait = 2 * time.Second
	sessionLockPoll = 5 * time.Millisecond
)

func saveSessions(book sessionBook) error {
	p, err := sessionsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	book.Version = 1
	if book.Bound == nil {
		book.Bound = map[string]string{}
	}
	b, err := json.MarshalIndent(book, "", "  ")
	if err != nil {
		return err
	}
	// Write-then-rename, so an interrupted save cannot truncate the book and
	// orphan every named session at once.
	//
	// The temp name is UNIQUE PER WRITER, and that is not paranoia. Every
	// `rindler run` is its own process, so two concurrent runs are two concurrent
	// writers with no lock available between them. On a SHARED temp path they
	// interleave: writer A truncates the file, writer B renames it into place
	// half-written, and the book that lands is unreadable -- which loadSessions
	// then reads as EMPTY, losing every name at once rather than one of them.
	// Measured once at 20 concurrent binds leaving 0 survivors on a shared path.
	// NOT test-pinned: the window is narrow enough that a deterministic test for
	// it did not reproduce, so this guard is reasoned rather than proven. It is
	// kept because it is free and obviously sound; the property that IS pinned is
	// the weaker and more important one, that a lock-bypassed concurrent write
	// cannot leave the book unreadable.
	//
	// With a unique temp, each rename publishes a COMPLETE book. Two writers can
	// still lose one another's addition (last rename wins), which is a real but
	// small cost: the losing name simply re-binds on next use, and the browser it
	// pointed at is idle-reaped. Total loss is what had to go.
	tmp := fmt.Sprintf("%s.tmp.%d.%d", p, os.Getpid(), time.Now().UnixNano())
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp) // never leave a stray temp behind on a failed publish
		return err
	}
	return nil
}

// sessionIDFor returns the id currently bound to a name, if any.
func sessionIDFor(name string) string {
	if strings.TrimSpace(name) == "" {
		return ""
	}
	return loadSessions().Bound[normalizeSessionName(name)]
}

// bindSession records that a name now refers to an id. Used both on first use
// and on re-attach, which are deliberately the same operation: a caller asking
// for a name wants a session called that, not a specific browser.
func bindSession(name, sessionID string) error {
	name = normalizeSessionName(name)
	if name == "" || sessionID == "" {
		return nil
	}
	// Locked read-modify-write: re-read INSIDE the lock, so a concurrent bind's
	// entry is merged rather than clobbered by a book we loaded before it landed.
	return withSessionBookLock(func() error {
		book := loadSessions()
		book.Bound[name] = sessionID
		return saveSessions(book)
	})
}

func unbindSession(name string) error {
	name = normalizeSessionName(name)
	return withSessionBookLock(func() error {
		book := loadSessions()
		if _, ok := book.Bound[name]; !ok {
			return nil
		}
		delete(book.Bound, name)
		return saveSessions(book)
	})
}

// normalizeSessionName trims and lowercases. Case-insensitive because a name is
// typed by a human at a shell, and "Trip" failing to find "trip" would be a
// puzzle with no error message -- it would just silently open a second browser.
func normalizeSessionName(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

// nextAutoName returns the lowest unused numeric name, tmux-style: 0, then 1,
// then 2. Lowest UNUSED rather than highest-plus-one, so killing session 0 makes
// "0" available again instead of counting away from zero forever.
func nextAutoName(book sessionBook) string {
	for i := 0; i < 10_000; i++ {
		name := strconv.Itoa(i)
		if _, taken := book.Bound[name]; !taken {
			return name
		}
	}
	// Unreachable in practice; a name is still better than an empty string,
	// which would silently mean "unnamed" and bind nothing.
	return "overflow"
}

// sortedSessionNames orders the book for display: numeric names numerically
// (so 2 comes before 10), then everything else alphabetically.
func sortedSessionNames(book sessionBook) []string {
	names := make([]string, 0, len(book.Bound))
	for name := range book.Bound {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		ni, iErr := strconv.Atoi(names[i])
		nj, jErr := strconv.Atoi(names[j])
		switch {
		case iErr == nil && jErr == nil:
			return ni < nj
		case iErr == nil:
			return true
		case jErr == nil:
			return false
		default:
			return names[i] < names[j]
		}
	})
	return names
}

func runSessions(args []string) int {
	if len(args) > 0 && args[0] != "--json" {
		fmt.Fprintln(os.Stderr, "usage: rindler sessions [--json]")
		return 2
	}
	book := loadSessions()
	if args != nil && len(args) == 1 && args[0] == "--json" {
		b, _ := json.MarshalIndent(book.Bound, "", "  ")
		fmt.Println(string(b))
		return 0
	}
	if len(book.Bound) == 0 {
		fmt.Println("No named sessions on this machine.")
		fmt.Println("Start one:  rindler run --session mywork --site <d> --action <a>")
		return 0
	}
	fmt.Println("Named sessions on this machine:")
	for _, name := range sortedSessionNames(book) {
		fmt.Printf("  %-20s %s\n", name, book.Bound[name])
	}
	fmt.Println("\nA session ends when you `rindler kill <name>`, or when the server")
	fmt.Println("reaps it for idleness. A name whose session is gone re-binds to a")
	fmt.Println("fresh one on next use.")
	return 0
}

func runKill(args []string) int {
	if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
		fmt.Fprintln(os.Stderr, "usage: rindler kill <session-name>")
		return 2
	}
	name := normalizeSessionName(args[0])
	id := sessionIDFor(name)
	if id == "" {
		fmt.Fprintf(os.Stderr, "no session named %q on this machine\n", name)
		return 1
	}
	// Drop the binding EVEN IF the close fails. The caller asked to be rid of
	// this name; leaving it bound to a session they believe is gone is the worse
	// outcome, and the server's idle reaper is the backstop for the browser
	// itself. Same reasoning as `vault disable` erasing local state regardless.
	closeErr := closeRemoteSession(id)
	if err := unbindSession(name); err != nil {
		fmt.Fprintln(os.Stderr, "kill:", err)
		return 1
	}
	if closeErr != nil {
		fmt.Printf("✓ Dropped %q locally.\n", name)
		fmt.Fprintf(os.Stderr, "⚠ Could not close it on the server: %v\n", closeErr)
		fmt.Fprintln(os.Stderr, "  It will be reaped for idleness.")
		return 0
	}
	fmt.Printf("✓ Closed %q.\n", name)
	return 0
}

// closeRemoteSession releases the browser server-side via the same endpoint
// frontends call on tab close. Mounted on the shared user-bearer options, so a
// rindler_live_ key is accepted, and fenced by the same privacy.SessionFence as
// every other by-id session door -- this CLI cannot close a colleague's session
// any more than it can drive one.
func closeRemoteSession(sessionID string) error {
	key, apiBase, code := resolveKeyAndBaseQuiet("")
	if code != 0 || key == "" {
		return fmt.Errorf("not logged in")
	}
	body, _ := json.Marshal(map[string]string{"session_id": sessionID})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(apiBase, "/")+"/api/sessions/close", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	res, err := defaultHTTPClient().Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	// The route answers {"ok":true} even when the session was already gone,
	// which is the outcome we wanted anyway.
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		return nil
	}
	return fmt.Errorf("server returned %s", errBody(res))
}
