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
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
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
	book := loadSessions()
	book.Bound[name] = sessionID
	return saveSessions(book)
}

func unbindSession(name string) error {
	name = normalizeSessionName(name)
	book := loadSessions()
	if _, ok := book.Bound[name]; !ok {
		return nil
	}
	delete(book.Bound, name)
	return saveSessions(book)
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
