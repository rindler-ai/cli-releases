package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// `rindler usage` — your own usage, read from the same endpoint and under the
// same rules as the dashboard's Usage tab.
//
// It deliberately mirrors rather than re-derives. The dashboard already settled
// the hard parts (one grouped query, the caller's own row resolved server-side
// from the verified key, a named remainder for unattributed work), and a second
// implementation would produce a second answer to the same question. The one
// nobody was looking at would be the wrong one.
//
// The FIRST cut of this file invented an envelope the server does not send
// ({scope, credits{remaining,used,allotment}, sessions, sites}) and its tests
// asserted that invention right back. Every field decoded to a zero value, and
// because a zero decodes without error the command printed a confident "0 used"
// and exited 0. The types below are transcribed from the real response, and
// envelopeLooksReal is the guard that makes the next drift LOUD instead of zero.

const (
	scopeMe        = "me"
	scopeWorkspace = "workspace"

	// The disclosures the dashboard shows. They are true of the DATA, not of the
	// surface, so the CLI owes the reader the same two sentences.
	creditsReconstructedNote = "Per-member credits are reconstructed from telemetry, not read from a per-member ledger."
	alsoVisibleNote          = "Owners and admins can see these same numbers."
)

// usageRow mirrors the server's memberUsageRow.
type usageRow struct {
	Actor       string  `json:"actor"`
	Actions     int64   `json:"actions"`
	Successes   int64   `json:"successes"`
	Blocked     int64   `json:"blocked"`
	SuccessRate float64 `json:"success_rate"`
	Credits     int64   `json:"credits"`
}

// usageTotals mirrors the server's memberUsageTotals: the sum over EVERY row,
// attributed and not. Note this is the WHOLE WORKSPACE, not just you.
type usageTotals struct {
	Actions   int64 `json:"actions"`
	Successes int64 `json:"successes"`
	Blocked   int64 `json:"blocked"`
	Credits   int64 `json:"credits"`
}

// usageResponse mirrors the server's memberUsageSelfResponse field for field.
// Keep it that way: this is a wire contract with a private module we cannot
// import, so the only thing standing between a server rename and a silently
// zeroed report is this struct plus the test that pins a real body.
type usageResponse struct {
	WindowDays int    `json:"window_days"`
	StartAt    string `json:"start_at"`
	EndAt      string `json:"end_at"`
	// Mine is zeroed, not absent, when you have no recorded usage — a real zero
	// a member is entitled to see, which is exactly why it cannot be used to
	// detect a decode failure. That is envelopeLooksReal's job.
	Mine                 usageRow    `json:"mine"`
	WorkspaceTotals      usageTotals `json:"workspace_totals"`
	Unattributed         usageRow    `json:"unattributed"`
	CreditsReconstructed bool        `json:"credits_reconstructed"`
	VisibleToAdmins      bool        `json:"visible_to_admins"`
}

// envelopeLooksReal separates "the server answered, and you genuinely did
// nothing this window" from "we decoded the wrong shape". Both produce zeroed
// counters, so the counters cannot tell them apart. The window fields can: the
// server stamps them on every response and they are never empty, whereas a
// mismatched envelope leaves them at their zero values.
func envelopeLooksReal(u usageResponse) bool {
	return u.WindowDays > 0 && u.EndAt != ""
}

func runUsage(args []string) int {
	fs := flag.NewFlagSet("usage", flag.ContinueOnError)
	apiBaseFlag := fs.String("api-base", "", "Rindler API origin")
	workspace := fs.Bool("workspace", false, "show the workspace totals instead of just yours")
	days := fs.Int("days", 0, "window in days (server default when unset)")
	jsonOut := fs.Bool("json", false, "print the raw JSON")
	if _, err := parseAnyOrder(fs, args); err != nil {
		return 2
	}
	key, apiBase, code := resolveKeyAndBase(*apiBaseFlag, "usage")
	if code != 0 {
		return code
	}

	// `days` is the ONLY parameter this endpoint reads. Which member you are is
	// resolved from the verified key server-side, so there is nothing here to
	// widen — and `--workspace` is a display choice made below, not a request
	// for someone else's data: the response carries both figures either way.
	endpoint := strings.TrimRight(apiBase, "/") + "/api/workspace/usage/me"
	if *days > 0 {
		endpoint += fmt.Sprintf("?days=%d", *days)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "usage:", err)
		return 1
	}
	req.Header.Set("Authorization", "Bearer "+key)
	res, err := defaultHTTPClient().Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "usage:", err)
		return 1
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if res.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "usage:", runAuthError(res.StatusCode, string(body)))
		return 1
	}
	if *jsonOut {
		fmt.Println(string(body))
		return 0
	}

	var u usageResponse
	if err := json.Unmarshal(body, &u); err != nil {
		fmt.Fprintln(os.Stderr, "usage: unreadable response")
		return 1
	}
	// FAIL LOUD, DO NOT RENDER ZEROS. A wrong envelope decodes cleanly into an
	// all-zero struct, and printing that is worse than printing nothing: it
	// looks like an answer.
	if !envelopeLooksReal(u) {
		fmt.Fprintln(os.Stderr,
			"usage: the server's response is missing its window; this CLI is likely older than the API. Update with `rindler upgrade`.")
		return 1
	}

	scope := scopeMe
	if *workspace {
		scope = scopeWorkspace
	}
	printUsage(os.Stdout, u, scope)
	return 0
}

func printUsage(w io.Writer, u usageResponse, scope string) {
	row := u.Mine
	heading := "Your usage"
	if scope == scopeWorkspace {
		heading = "Workspace usage"
		row = usageRow{
			Actions:   u.WorkspaceTotals.Actions,
			Successes: u.WorkspaceTotals.Successes,
			Blocked:   u.WorkspaceTotals.Blocked,
			Credits:   u.WorkspaceTotals.Credits,
			// The workspace figure carries no rate of its own; derive it from
			// the same two numbers rather than leaving a silent 0.0.
			SuccessRate: rate(u.WorkspaceTotals.Successes, u.WorkspaceTotals.Actions),
		}
	}

	if u.WindowDays > 0 {
		fmt.Fprintf(w, "%s — last %d days (through %s)\n\n", heading, u.WindowDays, dayOf(u.EndAt))
	} else {
		fmt.Fprintf(w, "%s\n\n", heading)
	}

	fmt.Fprintf(w, "  actions   %d\n", row.Actions)
	fmt.Fprintf(w, "  succeeded %d (%.0f%%)\n", row.Successes, row.SuccessRate*100)
	// Blocked is not a failure: it is the rules doing their job. Naming it that
	// way stops a healthy number from reading like an outage.
	fmt.Fprintf(w, "  blocked   %d (by your rules)\n", row.Blocked)
	fmt.Fprintf(w, "  credits   %d spent\n", row.Credits)

	// A member whose personal figure is small is owed the reason: work that ran
	// under no attributable actor lands here rather than vanishing.
	if scope == scopeMe && u.Unattributed.Actions > 0 {
		fmt.Fprintf(w, "\n  %d more actions ran unattributed (scheduled or automated work).\n",
			u.Unattributed.Actions)
	}
	if scope == scopeMe && u.WorkspaceTotals.Actions > 0 {
		fmt.Fprintf(w, "  Workspace total for the same window: %d actions.\n", u.WorkspaceTotals.Actions)
	}

	fmt.Fprintln(w)
	if u.CreditsReconstructed {
		fmt.Fprintf(w, "  %s\n", creditsReconstructedNote)
	}
	if u.VisibleToAdmins {
		fmt.Fprintf(w, "  %s\n", alsoVisibleNote)
	}
}

// dayOf trims an RFC3339 timestamp to its date. A usage window is a span of
// days, so a midnight time component is noise the reader has to look past.
// Anything that is not a timestamp is passed through untouched rather than
// truncated to its first ten characters.
func dayOf(ts string) string {
	if t, err := time.Parse(time.RFC3339, ts); err == nil {
		return t.Format("2006-01-02")
	}
	return ts
}

func rate(successes, actions int64) float64 {
	if actions <= 0 {
		return 0
	}
	return float64(successes) / float64(actions)
}
