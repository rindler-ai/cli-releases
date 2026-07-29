// `rindler map <url>` — start a site mapping run and follow it to a verdict.
//
// Wire contract (server): POST <api>/v1/runtime/map {url, mode} -> {job_id}, then
// GET <api>/v1/runtime/map/status/{job_id} -> {status, message, envelope{domain}}.
// Both sit behind the same per-key mapper authorization, which is why `rindler
// login` requests mapping by default (see login.go).

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// mapPollInterval is how often the status endpoint is polled while a run is in
// flight. A mapping crawl runs for minutes, so a tight loop would be pure noise
// against the API for no better answer.
const mapPollInterval = 5 * time.Second

type mapStartResponse struct {
	JobID string `json:"job_id"`
}

// mapStatusResponse mirrors what MapHandler.HandleStatus actually writes.
//
// The critical field is acceptance_state, and reading `status` without it is
// the bug this struct was rewritten to fix: the server reports top-level
// status "complete" for a mapping whose acceptance verdict was REJECTED,
// BLOCKED, NOT_PROVEN or SUPERSEDED (durableMappingHTTPStatus). "Complete"
// means the generation finished, not that the map is good. The verdict lives
// in acceptance_state, and whether the config actually reached the catalog
// lives in publication_state.
//
// Domain is top level. An earlier version of this struct read it from a nested
// `envelope` object, which the status endpoint has never sent -- that shape is
// the SITE-ENGINE's reply to the server, not the server's reply to us. So the
// field was always empty and every success line said "the site". The nested
// form is kept only as a fallback for the legacy no-durable-request lane.
type mapStatusResponse struct {
	Status           string `json:"status"`
	Message          string `json:"message"`
	Error            string `json:"error"`
	Domain           string `json:"domain"`
	AcceptanceState  string `json:"acceptance_state"`
	PublicationState string `json:"publication_state"`
	Envelope         struct {
		Domain string `json:"domain"`
	} `json:"envelope"`
}

// site names the mapped site, preferring the top-level field the server sends.
func (s mapStatusResponse) site() string {
	if d := strings.TrimSpace(s.Domain); d != "" {
		return d
	}
	if d := strings.TrimSpace(s.Envelope.Domain); d != "" {
		return d
	}
	return "the site"
}

// terminalAcceptanceStates are verdicts the acceptance lane will not revisit.
// Transcribed from the dashboard's MapSitePanel, which is the reference
// consumer of this same endpoint.
var terminalAcceptanceStates = map[string]bool{
	"rejected": true, "blocked": true, "budget_exhausted": true,
	"unrepairable": true, "not_proven": true, "superseded": true,
}

// accepted reports whether a finished mapping is one you can actually use.
//
// A map is only a win when acceptance ACCEPTED it and publication put it in
// the catalog. Anything else finished without publishing anything, so the next
// `rindler run` against that site fails with "unsupported site" -- after we
// told the user it worked.
//
// When the server sends no acceptance_state at all (the legacy lane, which has
// no durable request behind it) there is no verdict to consult and a terminal
// "complete" is the only signal there is. Treat that as success rather than
// failing every caller on that lane.
func (s mapStatusResponse) accepted() bool {
	if strings.TrimSpace(s.AcceptanceState) == "" {
		return true
	}
	if s.AcceptanceState != "accepted" {
		return false
	}
	switch strings.TrimSpace(s.PublicationState) {
	case "published", "not_applicable", "":
		return true
	default:
		return false
	}
}

// rejectionReason explains a finished-but-not-accepted map. The server already
// puts the verifier's own text in `message` for the rejected verdicts, so
// prefer that and fall back to naming the state.
func (s mapStatusResponse) rejectionReason() string {
	if m := strings.TrimSpace(s.Message); m != "" {
		return m
	}
	if e := strings.TrimSpace(s.Error); e != "" {
		return e
	}
	switch strings.TrimSpace(s.AcceptanceState) {
	case "rejected":
		return "the verifier rejected the mapping"
	case "blocked":
		return "the site blocked the mapping run"
	case "not_proven":
		return "the mapping could not be proven to work"
	case "budget_exhausted":
		return "the mapping ran out of budget before it could be proven"
	case "unrepairable":
		return "the mapping failed in a way the repair lane cannot fix"
	case "superseded":
		return "a newer mapping replaced this one"
	case "":
		return "the mapping did not finish successfully"
	default:
		return "the mapping ended as " + s.AcceptanceState
	}
}

// mapTerminal reports whether a status string ends the run, and whether it won.
// mapTerminal decides when to stop polling. Its terminal set MIRRORS the
// server's own terminal-status predicate, and must keep
// mirroring it, because a status the server considers finished but this
// function does not is a poll that never ends: the job is already over, so the
// status will never change again, and we sit there until --timeout and then
// report a timeout that did not happen.
//
// Two were missing, both genuinely terminal server-side:
//
//	expired           the request exceeded its maximum age and was finalized
//	needs_escalation  an operator step is pending; work has STOPPED, and the
//	                  server's own comment records this exact disagreement
//	                  stranding jobs once already
//
// Unknown statuses stay non-terminal on purpose: a new intermediate state
// should be waited through, not guessed at. That is safe only because the
// caller bounds the wait, which is why this returns "keep going" rather than
// looping itself.
func mapTerminal(status string) (done bool, ok bool) {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "complete", "completed", "success", "succeeded", "done":
		return true, true
	case "error", "failed", "failure", "expired", "needs_escalation",
		"cancelled", "canceled", "timeout":
		return true, false
	default:
		return false, false
	}
}

// mapStatusExplanation gives a terminal failure a reason a reader can act on.
// "failed" alone sends someone hunting for a bug in their own URL when the
// truth is that the job aged out or is sitting in a human queue.
func mapStatusExplanation(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "expired":
		return "the mapping request aged out before it finished; re-run it"
	case "needs_escalation":
		return "this site needs a person to look at it; the mapping did not finish on its own"
	default:
		return ""
	}
}

// normalizeMapTarget accepts what a human types ("example.com") and returns a URL
// the server will accept. A bare host is assumed https.
func normalizeMapTarget(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", fmt.Errorf("no URL given")
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("not a valid URL: %s", raw)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("unsupported scheme %q (use http or https)", u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("not a valid URL: %s", raw)
	}
	return u.String(), nil
}

// mapAuthError turns the server's authorization refusals into the actionable
// sentence each one actually implies, instead of a bare status code.
// mapRetryableError is a "not now" -- the run is fine and the next poll will
// get an answer. Distinct from a refusal because retrying one works and
// retrying the other just repeats the same no.
type mapRetryableError struct{ reason string }

func (e *mapRetryableError) Error() string { return e.reason }

func mapAuthError(code int, body string) error {
	se := decodeServerError(body)

	// 409 is TWO different things, told apart only by the body: "mapping status
	// changed; retry" is a race the next poll resolves, while "another mapping
	// already owns this domain" is a real refusal. Aborting on the first turned
	// a momentary race into a failed map for a run that was still going.
	if code == http.StatusConflict {
		if strings.Contains(strings.ToLower(se.Error), "retry") {
			return &mapRetryableError{reason: firstNonEmpty(se.Error, "mapping status changed; retry")}
		}
		return fmt.Errorf("%s", firstNonEmpty(se.Error, "another mapping already owns this domain"))
	}

	fix := ""
	switch code {
	case http.StatusUnauthorized:
		fix = "run `rindler login`"
	case http.StatusServiceUnavailable:
		// Only reached when the body is empty. Hedged deliberately: this code
		// covers both a deployment with no mapper and a momentary fault, and the
		// old text asserted the first.
		fix = "the mapping service did not answer; it may not be available on this deployment, or may be briefly down"
	case http.StatusForbidden:
		fix = "run `rindler status`: if it says (runtime) rather than (runtime + mapping), " +
			"log in again — and if mapping is still absent, your workspace is not entitled to it"
	}

	// 503 covers BOTH "this deployment has no mapper" and transient faults like
	// "could not read mapping state". Collapsing them into the first told people
	// their deployment could not map when the server had merely hiccuped, so the
	// server's own sentence is the one that gets printed.
	switch {
	case se.Error != "" && fix != "":
		return fmt.Errorf("%s — %s", se.Error, fix)
	case se.Error != "":
		return fmt.Errorf("%s (HTTP %d)", se.Error, code)
	case fix != "":
		return fmt.Errorf("map failed (HTTP %d) — %s", code, fix)
	}
	return fmt.Errorf("map failed (HTTP %d): %s", code, strings.TrimSpace(body))
}

func runMap(args []string) int {
	// `rindler map status <jobId>` — resume following a run started with --no-wait
	// or abandoned by a timeout. Both of those paths tell the user to run exactly
	// this, so it has to exist.
	if len(args) >= 1 && args[0] == "status" {
		return runMapStatus(args[1:])
	}
	fs := flag.NewFlagSet("map", flag.ContinueOnError)
	mode := fs.String("mode", "fast", "mapping depth: fast or deep")
	apiBaseFlag := fs.String("api-base", "", "Rindler API origin (defaults to the one you logged in against)")
	timeout := fs.Duration("timeout", 30*time.Minute, "how long to follow the run before giving up")
	noWait := fs.Bool("no-wait", false, "start the run and print the job id instead of following it")
	rest, err := parseAnyOrder(fs, args)
	if err != nil {
		return 2
	}
	if len(rest) < 1 {
		fmt.Fprintln(os.Stderr, "usage: rindler map <url> [--mode fast|deep] [--no-wait]")
		return 2
	}
	// Validate --mode rather than let the server coerce it: a typo'd `--mode Deep`
	// silently downgraded an expensive deep map to a fast one, and the user waited
	// for (and was billed for) a shallower config than they asked for.
	if m := strings.ToLower(strings.TrimSpace(*mode)); m != "fast" && m != "deep" {
		fmt.Fprintf(os.Stderr, "unknown --mode %q (want: fast or deep)\n", *mode)
		return 2
	}
	target, err := normalizeMapTarget(rest[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "map:", err)
		return 2
	}

	cfg, _ := loadConfig()
	store, _, err := newCredentialStore()
	if err != nil {
		fmt.Fprintln(os.Stderr, "map:", err)
		return 1
	}
	key, _, err := resolveActiveKey(store)
	if err != nil {
		fmt.Fprintln(os.Stderr, "map:", err)
		return 1
	}
	if key == "" {
		fmt.Fprintln(os.Stderr, "not logged in — run `rindler login` first (or set RINDLER_API_KEY)")
		return 1
	}
	// Shared resolver, so RINDLER_API_BASE reaches this verb too. This was the
	// THIRD hand-rolled copy of the precedence and, like the others, it skipped
	// the env var straight to a production default.
	apiBase := resolveAPIBase(*apiBaseFlag, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	httpc := defaultHTTPClient()

	jobID, err := startMap(ctx, httpc, apiBase, key, target, *mode)
	if err != nil {
		fmt.Fprintln(os.Stderr, "map:", err)
		return 1
	}
	fmt.Printf("Mapping %s (%s)…\njob %s\n", target, *mode, jobID)
	if *noWait {
		fmt.Printf("Follow it with: rindler map status %s\n", jobID)
		return 0
	}
	return followMap(ctx, httpc, apiBase, key, jobID)
}

// runMapStatus follows an already-started run by job id.
func runMapStatus(args []string) int {
	fs := flag.NewFlagSet("map status", flag.ContinueOnError)
	apiBaseFlag := fs.String("api-base", "", "Rindler API origin (defaults to the one you logged in against)")
	timeout := fs.Duration("timeout", 30*time.Minute, "how long to follow the run before giving up")
	once := fs.Bool("once", false, "print the current status and exit instead of following")
	rest, err := parseAnyOrder(fs, args)
	if err != nil {
		return 2
	}
	if len(rest) < 1 {
		fmt.Fprintln(os.Stderr, "usage: rindler map status <job-id> [--once]")
		return 2
	}
	jobID := rest[0]

	cfg, _ := loadConfig()
	store, _, err := newCredentialStore()
	if err != nil {
		fmt.Fprintln(os.Stderr, "map status:", err)
		return 1
	}
	key, _, err := resolveActiveKey(store)
	if err != nil || key == "" {
		fmt.Fprintln(os.Stderr, "not logged in — run `rindler login` first (or set RINDLER_API_KEY)")
		return 1
	}
	// resolveAPIBase, NOT a hand-rolled chain: this one skipped
	// RINDLER_API_BASE, so `map status` sent the Bearer key to PRODUCTION even
	// when every other verb was pointed at a dev/self-hosted lane -- and it is the
	// exact command the CLI tells you to run after --no-wait and after every
	// follow timeout.
	apiBase := resolveAPIBase(*apiBaseFlag, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	httpc := defaultHTTPClient()

	if *once {
		st, err := mapStatus(ctx, httpc, apiBase, key, jobID)
		if err != nil {
			fmt.Fprintln(os.Stderr, "map status:", err)
			return 1
		}
		fmt.Println(strings.TrimSpace(st.Status + " " + st.Message))
		// Same verdict as the follow path. A one-shot check that disagreed with
		// the thing it is a shortcut for would be worse than not having it.
		if done, ok := mapTerminal(st.Status); done {
			if !ok || !st.accepted() {
				if ok {
					fmt.Fprintf(os.Stderr, "not usable: %s\n", st.rejectionReason())
				}
				return 1
			}
		}
		return 0
	}
	return followMap(ctx, httpc, apiBase, key, jobID)
}

func startMap(ctx context.Context, httpc *http.Client, apiBase, key, target, mode string) (string, error) {
	payload, _ := json.Marshal(map[string]string{"url": target, "mode": mode})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiBase+"/v1/runtime/map", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	res, err := httpc.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusAccepted {
		return "", mapAuthError(res.StatusCode, string(body))
	}
	var out mapStartResponse
	if err := json.Unmarshal(body, &out); err != nil || out.JobID == "" {
		return "", fmt.Errorf("server did not return a job id: %s", strings.TrimSpace(string(body)))
	}
	return out.JobID, nil
}

// followMap polls until the run reaches a terminal status, printing each change.
func followMap(ctx context.Context, httpc *http.Client, apiBase, key, jobID string) int {
	last := ""
	for {
		st, err := mapStatus(ctx, httpc, apiBase, key, jobID)
		if err != nil {
			// A transient read failure must not look like a failed MAP: the run is
			// still going server-side, so say what actually happened and keep the
			// job id recoverable.
			if ctx.Err() != nil {
				fmt.Fprintf(os.Stderr, "\nstopped following after the timeout; the run may still be going.\n"+
					"Check it with: rindler map status %s\n", jobID)
				return 1
			}
			// A retry-me is the server asking us to look again, not a verdict.
			var retryable *mapRetryableError
			if errors.As(err, &retryable) {
				select {
				case <-ctx.Done():
					fmt.Fprintf(os.Stderr, "\nstopped following after the timeout; the run may still be going.\n"+
						"Check it with: rindler map status %s\n", jobID)
					return 1
				case <-time.After(mapPollInterval):
				}
				continue
			}
			fmt.Fprintln(os.Stderr, "map: status check failed:", err)
			return 1
		}
		line := strings.TrimSpace(st.Status + " " + st.Message)
		if line != last {
			fmt.Println(" ", line)
			last = line
		}
		if done, ok := mapTerminal(st.Status); done {
			// "complete" is about the GENERATION, not the verdict. A rejected,
			// blocked or unproven mapping also finishes "complete", and publishes
			// nothing -- so claiming success here sends the user to a `rindler run`
			// that fails with "unsupported site".
			if ok && st.accepted() {
				fmt.Printf("\n✓ Mapped %s.\n", st.site())
				return 0
			}
			if ok {
				fmt.Fprintf(os.Stderr, "\n✗ Mapping did not produce a usable config: %s\n",
					st.rejectionReason())
				return 1
			}
			reason := strings.TrimSpace(st.Message)
			// An aged-out or escalated job is not a mapping that "failed" in the
			// sense the reader will assume, and the server does not always send a
			// message. Without this they go looking for a mistake in their own URL.
			if why := mapStatusExplanation(st.Status); why != "" {
				if reason == "" {
					reason = why
				} else {
					reason += " (" + why + ")"
				}
			}
			if reason == "" {
				reason = st.Status
			}
			fmt.Fprintf(os.Stderr, "\n✗ Mapping failed: %s\n", reason)
			return 1
		}
		select {
		case <-ctx.Done():
			fmt.Fprintf(os.Stderr, "\nstopped following after the timeout; the run may still be going.\n"+
				"Check it with: rindler map status %s\n", jobID)
			return 1
		case <-time.After(mapPollInterval):
		}
	}
}

func mapStatus(ctx context.Context, httpc *http.Client, apiBase, key, jobID string) (mapStatusResponse, error) {
	var out mapStatusResponse
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		apiBase+"/v1/runtime/map/status/"+url.PathEscape(jobID), nil)
	if err != nil {
		return out, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	res, err := httpc.Do(req)
	if err != nil {
		return out, err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != http.StatusOK {
		return out, mapAuthError(res.StatusCode, string(body))
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return out, fmt.Errorf("unreadable status response: %s", strings.TrimSpace(string(body)))
	}
	return out, nil
}
