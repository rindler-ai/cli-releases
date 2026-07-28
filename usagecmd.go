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

// usageRow mirrors the server's per-member row, INCLUDING its outcome
// vocabulary. An earlier version of this struct declared six of the eleven
// fields and silently dropped the rest -- the same under-transcription that
// zeroed this whole command once already.
//
// The outcome fields are the ones worth having. "successes" counts calls that
// did not error; "completed" counts calls that actually finished the work, and
// "handed_off" counts the ones that stopped for an auth wall or a captcha --
// which for a CLI user is the difference between "it is broken" and "it needs
// you".
type usageRow struct {
	Actor     string `json:"actor"`
	Actions   int64  `json:"actions"`
	Successes int64  `json:"successes"`
	Blocked   int64  `json:"blocked"`

	Completed    int64 `json:"completed"`
	HandedOff    int64 `json:"handed_off"`
	Failed       int64 `json:"failed"`
	Unclassified int64 `json:"unclassified"`

	// SuccessRate is completed / (completed + failed) -- NOT successes/actions.
	// Printing it next to `successes` implied the two were one measurement; they
	// are not, and the pairing overstated the rate whenever work was handed off.
	SuccessRate  float64 `json:"success_rate"`
	Credits      int64   `json:"credits"`
	LastActiveAt string  `json:"last_active_at,omitempty"`
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
	Mine            usageRow    `json:"mine"`
	WorkspaceTotals usageTotals `json:"workspace_totals"`
	Unattributed    usageRow    `json:"unattributed"`
	// Sessions, TopActions and FailureShapes are the caller's own shape. All
	// three are OPTIONAL and separately failing server-side, so a nil is "not
	// available", never "zero" -- and must be omitted rather than rendered.
	Sessions *struct {
		Sessions int64 `json:"sessions"`
		MedianMs int64 `json:"median_ms"`
		P90Ms    int64 `json:"p90_ms"`
	} `json:"sessions,omitempty"`
	TopActions []struct {
		Action    string `json:"action"`
		Calls     int64  `json:"calls"`
		Succeeded int64  `json:"succeeded"`
		Failed    int64  `json:"failed"`
	} `json:"top_actions,omitempty"`
	FailureShapes []struct {
		Shape string `json:"shape"`
		Calls int64  `json:"calls"`
	} `json:"failure_shapes,omitempty"`

	CreditsReconstructed bool `json:"credits_reconstructed"`
	VisibleToAdmins      bool `json:"visible_to_admins"`
}

// envelopeLooksReal separates "the server answered, and you genuinely did
// nothing this window" from "we decoded the wrong shape". Both produce zeroed
// counters, so the counters cannot tell them apart. The window fields can: the
// server stamps them on every response and they are never empty, whereas a
// mismatched envelope leaves them at their zero values.
func envelopeLooksReal(u usageResponse) bool {
	return u.WindowDays > 0 && u.EndAt != ""
}

// creditsResponse mirrors the entitlements self endpoint's credit fields.
//
// `known` is the load-bearing one and the reason Credit is a POINTER: the
// server reports the pool verdict and the BALANCE separately, on purpose, so a
// failed balance read cannot flip who gets debited. That means "we could not
// read your balance" and "your balance is zero" arrive as different things and
// must stay different here -- printing a confident 0 remaining for a failed
// read is the same class of lie as the zeroed report this command already had.
type creditsResponse struct {
	Pool   string `json:"pool"`
	Credit *struct {
		Known     bool  `json:"known"`
		Remaining int64 `json:"remaining"`
		Allotment int64 `json:"allotment"`
		Used      int64 `json:"used"`
	} `json:"workspace_credit,omitempty"`
}

// fetchCredits reads the balance. Best-effort by design: usage numbers are
// worth printing on their own, so a credits read that fails degrades to
// omitting that line rather than failing the command.
func fetchCredits(ctx context.Context, apiBase, key string) *creditsResponse {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(apiBase, "/")+"/api/entitlements/self", nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+key)
	res, err := defaultHTTPClient().Do(req)
	if err != nil {
		return nil
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	var c creditsResponse
	if json.Unmarshal(body, &c) != nil {
		return nil
	}
	return &c
}

// creditBar renders remaining/allotment. Clamped at both ends so an over-spend
// or a bad allotment cannot draw a negative or runaway bar.
func creditBar(remaining, total int64) string {
	const width = 20
	if total <= 0 {
		return ""
	}
	filled := int(remaining * width / total)
	if filled < 0 {
		filled = 0
	}
	if filled > width {
		filled = width
	}
	return "[" + strings.Repeat("#", filled) + strings.Repeat("-", width-filled) + "]"
}

// burnRate is credits spent per day over the window. Derived here rather than
// asked of the server because both inputs are already on the wire -- a new
// endpoint for division would be a second place for the number to disagree.
//
// Returns 0 when it cannot be computed, and callers must treat that as "do not
// print", never as "zero burn": a window with no spend and a window we cannot
// measure look identical in the result and must not look identical on screen.
func burnRate(creditsSpent int64, windowDays int) float64 {
	if creditsSpent <= 0 || windowDays <= 0 {
		return 0
	}
	return float64(creditsSpent) / float64(windowDays)
}

// daysLeft projects how long the remaining balance lasts at the observed burn.
// Deliberately conservative about what it will claim:
//
//   - a zero or unknown burn yields 0 (no projection), because dividing by it
//     would produce infinity and print a promise
//   - it is capped, because "your credits last 9000 days" is noise, and a tiny
//     burn over a short window extrapolates absurdly far
func daysLeft(remaining int64, burn float64) int {
	if burn <= 0 || remaining <= 0 {
		return 0
	}
	d := int(float64(remaining) / burn)
	if d > 999 {
		return 999
	}
	return d
}

func printCredits(w io.Writer, c *creditsResponse) {
	fmt.Fprintln(w)
	if c == nil {
		fmt.Fprintln(w, "  credits  (could not read your balance)")
		return
	}
	if c.Credit == nil {
		// A personal-pool verdict carries no workspace balance. Say which pool
		// rather than implying the number is missing.
		fmt.Fprintf(w, "  credits  billed to the %s pool\n", firstNonEmptyStr(c.Pool, "personal"))
		return
	}
	if !c.Credit.Known {
		// The server told us it could not read the balance. That is NOT zero.
		fmt.Fprintln(w, "  credits  (balance temporarily unavailable)")
		return
	}
	if bar := creditBar(c.Credit.Remaining, c.Credit.Allotment); bar != "" {
		fmt.Fprintf(w, "  credits  %s  %d of %d left\n", bar, c.Credit.Remaining, c.Credit.Allotment)
		return
	}
	fmt.Fprintf(w, "  credits  %d used\n", c.Credit.Used)
}

func firstNonEmptyStr(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
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
		fmt.Fprintf(os.Stderr,
			"usage: the server's response is not the shape this CLI (%s) reads, so there are no numbers to show.\n"+
				"Reinstall to get a build that matches the API:\n  curl %s | sh\n",
			version, installURL)
		return 1
	}

	scope := scopeMe
	if *workspace {
		scope = scopeWorkspace
	}
	printUsage(os.Stdout, u, scope)
	// Credits last and separately: it is a different read against a different
	// endpoint, and one failing must not take the other's numbers down with it.
	credits := fetchCredits(ctx, apiBase, key)
	printCredits(os.Stdout, credits)
	printBurn(os.Stdout, u, credits)
	printSelfShape(os.Stdout, u)
	printDisclosures(os.Stdout, u)
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

	fmt.Fprintf(w, "  actions      %d\n", row.Actions)
	// The outcome vocabulary, in the order a reader cares about. The rate belongs
	// to `completed`, since that is what it is derived from.
	// The rate is the server's, derived from completed/(completed+failed). It is
	// printed against `completed` because that is its numerator -- putting it
	// beside `successes`, as this once did, paired two different measurements.
	fmt.Fprintf(w, "  completed    %d (%.0f%% success rate)\n",
		row.Completed, row.SuccessRate*100)
	if row.HandedOff > 0 {
		// The one people most need: not broken, waiting on a human.
		fmt.Fprintf(w, "  handed back  %d (an auth wall or captcha needed you)\n", row.HandedOff)
	}
	if row.Failed > 0 {
		fmt.Fprintf(w, "  failed       %d\n", row.Failed)
	}
	// Blocked is not a failure: it is the rules doing their job. Naming it that
	// way stops a healthy number from reading like an outage.
	if row.Blocked > 0 {
		fmt.Fprintf(w, "  blocked      %d (by your rules)\n", row.Blocked)
	}
	if row.Unclassified > 0 {
		// Named rather than folded into a bucket it does not belong to: these
		// predate the classifier and we genuinely do not know how they ended.
		fmt.Fprintf(w, "  unclassified %d (ran before outcomes were recorded)\n", row.Unclassified)
	}
	fmt.Fprintf(w, "  credits      %d spent\n", row.Credits)
	if scope == scopeMe && row.LastActiveAt != "" {
		fmt.Fprintf(w, "  last active  %s\n", dayOf(row.LastActiveAt))
	}

	// A member whose personal figure is small is owed the reason: work that ran
	// under no attributable actor lands here rather than vanishing.
	if scope == scopeMe && u.Unattributed.Actions > 0 {
		fmt.Fprintf(w, "\n  %d more actions ran unattributed (scheduled or automated work).\n",
			u.Unattributed.Actions)
	}
	if scope == scopeMe && u.WorkspaceTotals.Actions > 0 {
		fmt.Fprintf(w, "  Workspace total for the same window: %d actions.\n", u.WorkspaceTotals.Actions)
	}

}

// printDisclosures closes the report. Separated from printUsage so the numbers
// -- usage, then balance, then burn -- read as one block and the notes about
// them come after, rather than being buried between two sets of figures.
func printDisclosures(w io.Writer, u usageResponse) {
	if !u.CreditsReconstructed && !u.VisibleToAdmins {
		return
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

// printBurn reports the spend rate and how long the balance lasts at it.
//
// Both are derived from numbers already printed above, so they can only ever
// restate what is on screen -- which is the point: a reader should be able to
// check them. Anything it cannot compute honestly is omitted rather than
// rendered as a zero.
func printBurn(w io.Writer, u usageResponse, c *creditsResponse) {
	burn := burnRate(u.Mine.Credits, u.WindowDays)
	if burn <= 0 {
		return
	}
	fmt.Fprintf(w, "  burn     %.1f credits/day over the last %d days\n", burn, u.WindowDays)

	// A projection needs a balance we actually know. c.Credit.Known false means
	// the server could not read it, which is not zero and cannot be projected.
	if c == nil || c.Credit == nil || !c.Credit.Known {
		return
	}
	if d := daysLeft(c.Credit.Remaining, burn); d > 0 {
		if d >= 999 {
			fmt.Fprintln(w, "  runway   999+ days at this rate")
			return
		}
		fmt.Fprintf(w, "  runway   ~%d days at this rate\n", d)
	}
}

// printSelfShape renders the caller's session timings, busiest actions and the
// shapes their failures take. Every section is omitted when the server did not
// send it: these are separately-failing extra reads, so absent means "could not
// read", which must not be drawn as a zero.
func printSelfShape(w io.Writer, u usageResponse) {
	if u.Sessions == nil && len(u.TopActions) == 0 && len(u.FailureShapes) == 0 {
		return
	}
	if s := u.Sessions; s != nil && s.Sessions > 0 {
		fmt.Fprintf(w, "\n  sessions     %d (median %s, p90 %s)\n",
			s.Sessions, humanMs(s.MedianMs), humanMs(s.P90Ms))
	}
	if len(u.TopActions) > 0 {
		fmt.Fprintln(w, "\n  busiest actions")
		for _, a := range u.TopActions {
			line := fmt.Sprintf("    %-28s %d", a.Action, a.Calls)
			if a.Failed > 0 {
				line += fmt.Sprintf("  (%d failed)", a.Failed)
			}
			fmt.Fprintln(w, line)
		}
	}
	if len(u.FailureShapes) > 0 {
		// The shape is the actionable part: "12 failed" tells you nothing, "9 of
		// them were bot walls" tells you what to do next.
		fmt.Fprintln(w, "\n  how failures ended")
		for _, f := range u.FailureShapes {
			fmt.Fprintf(w, "    %-28s %d\n", f.Shape, f.Calls)
		}
	}
}

// humanMs renders a duration the way a reader thinks about it. Sub-second stays
// in milliseconds; past a minute, seconds alone stop being readable.
func humanMs(ms int64) string {
	switch {
	case ms <= 0:
		return "0s"
	case ms < 1000:
		return fmt.Sprintf("%dms", ms)
	case ms < 60_000:
		return fmt.Sprintf("%.1fs", float64(ms)/1000)
	default:
		return fmt.Sprintf("%dm%ds", ms/60_000, (ms%60_000)/1000)
	}
}
