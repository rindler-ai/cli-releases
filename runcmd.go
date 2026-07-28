// `rindler run` — execute structured actions against a site and follow the job
// to a verdict.
//
// Wire contract: POST <api>/v1/runtime/run {site|site_id, actions[], inputs{},
// idempotency_key} -> 202 {job_id}, then GET <api>/v1/runtime/jobs/{job_id} ->
// RuntimeJobEnvelope. Unlike the mapper, run accepts ANY key, so a plain
// `rindler login` is enough.
//
// The envelope deliberately reports TWO different things and this command keeps
// them apart:
//
//	status    — did the attempt run (queued/running/complete/succeeded/failed…)
//	retrieval — what the source actually held, and why it fell short
//
// Collapsing them is the documented bug the server split them to prevent: a bot
// wall, an expired cookie, or a rotted selector all finish the job "fine" while
// returning nothing, and reporting only the status would call that success.

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

const runPollInterval = 3 * time.Second

// stringList collects a repeatable flag (--action a --action b).
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

type runStartResponse struct {
	JobID string `json:"job_id"`
	ID    string `json:"id"`
}

// runOutputs mirrors the server's RecordsView. A NAMED type, not the anonymous
// struct this used to be: the shape grows as the server adds fields, and every
// growth of an anonymous struct breaks every literal that constructs it, which
// is friction that discourages exactly the field-adding this contract needs.
type runOutputs struct {
	Records   []map[string]any `json:"records"`
	Truncated bool             `json:"truncated,omitempty"`
	// Total is how many records EXIST, when the server knows. Without it
	// "records: 5" is unreadable: five of five and five of twelve hundred look
	// identical, and the second is the one worth knowing about.
	Total int `json:"total,omitempty"`
}

// runEvidence mirrors the server's EvidenceView: where to LOOK at the run. A
// truncated or walled result is exactly when someone wants the browser's own
// view, and this CLI was dropping a link the server already sends.
type runEvidence struct {
	RunViewerURL string   `json:"run_viewer_url,omitempty"`
	Screenshots  []string `json:"screenshots,omitempty"`
}

type retrievalView struct {
	Outcome       string   `json:"outcome"`
	Complete      bool     `json:"complete"`
	Reasons       []string `json:"reasons,omitempty"`
	RetryGuidance string   `json:"retry_guidance,omitempty"`
	FailureShape  string   `json:"failure_shape,omitempty"`
}

type runJobEnvelope struct {
	JobID  string `json:"job_id"`
	ID     string `json:"id"`
	Status string `json:"status"`
	Verb   string `json:"verb,omitempty"`
	// SessionID is the browser this run used. Populated for the run verb only,
	// and the one way a caller learns which session to reuse next time.
	SessionID string         `json:"session_id,omitempty"`
	Site      string         `json:"site,omitempty"`
	ErrMsg    string         `json:"error_msg,omitempty"`
	Outputs   *runOutputs    `json:"outputs,omitempty"`
	Evidence  *runEvidence   `json:"evidence,omitempty"`
	Retrieval *retrievalView `json:"retrieval,omitempty"`
	Usage     struct {
		OutcomeCount int32 `json:"outcome_count"`
		Steps        int32 `json:"steps,omitempty"`
	} `json:"usage"`
	Error *struct {
		Class   string `json:"class,omitempty"`
		Message string `json:"message,omitempty"`
	} `json:"error,omitempty"`
}

// runTerminal decides when to stop polling a run. An UNRECOGNISED status is
// deliberately non-terminal: treating an unknown value as done would end the
// poll early and report whatever partial state happened to be there. Same contract, and same
// hazard, as mapTerminal: a status the server treats as finished but this does
// not is an infinite poll, because a finished job's status never changes again.
// "needs_escalation" is terminal server-side and was missing here too.
func runTerminal(status string) (done bool, ok bool) {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "complete", "completed", "succeeded", "success":
		return true, true
	case "failed", "error", "expired", "needs_escalation",
		"cancelled", "canceled", "timeout", "timed_out":
		return true, false
	default:
		return false, false
	}
}

// runExitCode is the ONE place a finished job becomes an exit code.
//
// Two verdicts decide it, and that is the whole reason the server reports both:
//
//	status     did the attempt RUN
//	retrieval  did it come back with what was asked for
//
// A job can finish "complete" having retrieved nothing usable -- a bot wall, an
// expired cookie, a rotted selector -- and to a script that is not a success.
//
// It is one function because it used to be two, and they disagreed: the follow
// path weighed retrieval and `run status --once` did not, so the same finished
// job exited 1 when followed and 0 when polled. A script using the shortcut read
// a walled run as a win. Same defect the map lane had, same fix.
//
// A job that has NOT finished is not a failure; the caller decides whether to
// keep waiting.
func runExitCode(env runJobEnvelope) int {
	done, ok := runTerminal(env.Status)
	if !done {
		return 0
	}
	if !ok {
		return 1
	}
	if env.Retrieval != nil && !env.Retrieval.Complete && !truncationOnly(env) {
		return 1
	}
	return 0
}

// truncationOnly reports whether the ONLY thing incomplete about a retrieval is
// that the list was capped.
//
// The server marks a run `complete: false` when it returned records AND hit the
// cap (retrieval_outcome.go: records > 0 && truncated -> OutcomePartial). That is
// the ordinary case for any site with more rows than the cap, which defaults to
// five -- so treating incompleteness alone as failure made a perfectly healthy
// `rindler run` exit 1, and would have broken every script that checks the
// status. A false failure is worse than the silent success it replaced.
//
// A capped page is what the caller asked for: they wanted a page, the truncation
// is already reported as a note, and --limit is how they ask for more. Anything
// ELSE incomplete -- a bot wall, an expired cookie, an unmet requirement -- still
// fails, which is the case the split verdict exists for.
func truncationOnly(env runJobEnvelope) bool {
	if env.Retrieval == nil || env.Outputs == nil {
		return false
	}
	// Records AND truncation, and no other stated reason. An empty result that is
	// merely truncated is NOT this case: nothing came back.
	if len(env.Outputs.Records) == 0 || !env.Outputs.Truncated {
		return false
	}
	for _, r := range env.Retrieval.Reasons {
		if !strings.Contains(strings.ToLower(r), "truncat") &&
			!strings.Contains(strings.ToLower(r), "cap") {
			return false
		}
	}
	return true
}

// parseInputs turns repeated k=v flags into the inputs map. An entry with no '='
// is a mistake worth refusing: silently dropping it would run the action with a
// missing parameter and blame the site for the empty result.
func parseInputs(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(pairs))
	for _, p := range pairs {
		k, v, found := strings.Cut(p, "=")
		k = strings.TrimSpace(k)
		if !found || k == "" {
			return nil, fmt.Errorf("--input must be key=value, got %q", p)
		}
		out[k] = v
	}
	return out, nil
}

// newIdempotencyKey binds one CLI invocation to one durable job, so a retried
// request cannot start a second run (the server requires the field).
func newIdempotencyKey() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("rindler-cli-%d", time.Now().UnixNano())
	}
	return "rindler-cli-" + hex.EncodeToString(b[:])
}

// siteFromTarget accepts a URL or a bare host and returns the host the `site`
// field wants.
func siteFromTarget(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", fmt.Errorf("no site given")
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("not a valid site or URL: %s", raw)
	}
	return u.Host, nil
}

func runRun(args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	var actions stringList
	var inputs stringList
	fs.Var(&actions, "action", "action to run (repeatable, ordered)")
	fs.Var(&inputs, "input", "action input as key=value (repeatable)")
	site := fs.String("site", "", "site domain or URL to run against")
	mode := fs.String("mode", "", "optional run mode passed through to the server")
	limit := fs.Int("limit", 0, "max records for a list action (0 = the site's default)")
	sessionName := fs.String("session", "", "reuse a named browser session (auto-numbered when omitted)")
	apiBaseFlag := fs.String("api-base", "", "Rindler API origin (defaults to the one you logged in against)")
	timeout := fs.Duration("timeout", 15*time.Minute, "how long to follow the run")
	noWait := fs.Bool("no-wait", false, "start the run and print the job id instead of following it")
	jsonOut := fs.Bool("json", false, "print the terminal job envelope as JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*site) == "" || len(actions) == 0 {
		fmt.Fprintln(os.Stderr, "usage: rindler run --site <domain> --action <name> [--action ...] [--input k=v] [--json]")
		return 2
	}
	host, err := siteFromTarget(*site)
	if err != nil {
		fmt.Fprintln(os.Stderr, "run:", err)
		return 2
	}
	inputMap, err := parseInputs(inputs)
	if err != nil {
		fmt.Fprintln(os.Stderr, "run:", err)
		return 2
	}

	key, apiBase, code := resolveKeyAndBase(*apiBaseFlag, "run")
	if code != 0 {
		return code
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	httpc := defaultHTTPClient()

	// Named sessions. The NAME is ours; the server only ever sees an id.
	name := normalizeSessionName(*sessionName)
	if name == "" {
		// Unnamed runs still get a name, tmux-style, so a follow-up can address
		// this session at all. Without one there would be nothing to reuse.
		name = nextAutoName(loadSessions())
	}
	jobID, sessionID, err := startRunInSession(
		ctx, httpc, apiBase, key, host, actions, inputMap, *mode, *limit, name)
	if err != nil {
		fmt.Fprintln(os.Stderr, "run:", err)
		return 1
	}
	// Already known when reusing; otherwise the follow loop binds it as soon as
	// the server reports which browser the run got.
	pending := name
	if sessionID != "" {
		pending = ""
		if bindErr := bindSession(name, sessionID); bindErr != nil {
			// A failed bind costs a future reuse, not this run. Say so and carry on.
			fmt.Fprintf(os.Stderr, "warning: could not remember session %q: %v\n", name, bindErr)
		}
	}
	if !*jsonOut {
		fmt.Printf("Running %s on %s…\njob %s\n", strings.Join(actions, ", "), host, jobID)
	}
	if *noWait {
		fmt.Printf("Follow it with: rindler run status %s\n", jobID)
		return 0
	}
	return followRun(ctx, httpc, apiBase, key, jobID, *jsonOut, pending)
}

// resolveKeyAndBase is the shared "am I logged in, and against what" step.
func resolveKeyAndBase(apiBaseFlag, verb string) (key, apiBase string, exitCode int) {
	cfg, _ := loadConfig()
	store, _, err := newCredentialStore()
	if err != nil {
		fmt.Fprintln(os.Stderr, verb+":", err)
		return "", "", 1
	}
	key, _, err = resolveActiveKey(store)
	if err != nil {
		fmt.Fprintln(os.Stderr, verb+":", err)
		return "", "", 1
	}
	if key == "" {
		fmt.Fprintln(os.Stderr, "not logged in — run `rindler login` first (or set RINDLER_API_KEY)")
		return "", "", 1
	}
	return key, resolveAPIBase(apiBaseFlag, cfg), 0
}

// resolveAPIBase picks the origin every authenticated command talks to:
// --api-base, then RINDLER_API_BASE, then the origin recorded at login, then
// the built-in default.
//
// It is ONE function because it used to be two, and they disagreed. The env
// var was read by `login` alone, even though the help text advertises it as
// "override the API origin" with no verb qualifier -- every other command
// skipped from the flag straight to the config. That was worst exactly where
// the variable is most used: in CI you set RINDLER_API_KEY and never log in,
// so there is no config to fall back to, and the fallback is the PRODUCTION
// default. The override was ignored and the commands quietly went to prod.
//
// Env beats config deliberately. The config is a leftover from whenever you
// last logged in; an env var is something you set for this shell, now.
func resolveAPIBase(flagValue string, cfg cliConfig) string {
	for _, candidate := range []string{
		flagValue,
		os.Getenv("RINDLER_API_BASE"),
		cfg.APIBase,
		defaultAPIBase,
	} {
		if v := strings.TrimRight(strings.TrimSpace(candidate), "/"); v != "" {
			return v
		}
	}
	return defaultAPIBase
}

// runAuthError names what each refusal means. Run accepts any key, so a 403 here
// is about the SITE, not the key — pointing the user at `rindler login` would
// send them to fix something that is not broken.
// serverError is the one error shape this API uses: {"error", "class"}, from
// writeError/writeErrorClass.
type serverError struct {
	Error string `json:"error"`
	Class string `json:"class,omitempty"`
}

// decodeServerError pulls the server's own words out of a failure body. The
// body is the most specific thing available and was being thrown away on every
// classified status.
func decodeServerError(body string) serverError {
	var se serverError
	_ = json.Unmarshal([]byte(body), &se)
	se.Error = strings.TrimSpace(se.Error)
	return se
}

// runAuthError turns a failed response into something a reader can act on.
//
// The server's message wins when it sent one. These canned strings exist to
// add the FIX -- which command to run next -- to a status code that on its own
// says only "no". They were replacing the server's specific explanation with a
// generic guess, so a 403 for "this action needs a saved login" and a 403 for
// "the site is not in your catalog" read identically, and both blamed the
// catalog.
//
// verb is named so the message does not say "run failed" when it was `sites`
// or `actions` that failed; those share this mapper.
func runAuthError(code int, body string) error {
	return verbError("run", code, body)
}

func verbError(verb string, code int, body string) error {
	se := decodeServerError(body)
	fix := ""
	switch code {
	case http.StatusUnauthorized:
		fix = "run `rindler login`"
	case http.StatusForbidden:
		fix = "this is about the site, not your login: it is not in your catalog and you do not own a config for it"
	case http.StatusNotFound:
		fix = "map it first: rindler map <url>"
	case http.StatusTooManyRequests:
		fix = "rate limited or out of quota; try again shortly"
	}
	switch {
	case se.Error != "" && fix != "":
		return fmt.Errorf("%s — %s", se.Error, fix)
	case se.Error != "":
		return fmt.Errorf("%s (HTTP %d)", se.Error, code)
	case fix != "":
		return fmt.Errorf("%s failed (HTTP %d) — %s", verb, code, fix)
	}
	return fmt.Errorf("%s failed (HTTP %d): %s", verb, code, strings.TrimSpace(body))
}

// startRun is the no-session form, kept for callers that never reuse one.
func startRun(
	ctx context.Context, httpc *http.Client, apiBase, key, site string,
	actions []string, inputs map[string]string, mode string, limit int,
) (string, error) {
	return startRunWithSession(ctx, httpc, apiBase, key, site, actions, inputs, mode, limit, "", false)
}

func startRunWithSession(
	ctx context.Context, httpc *http.Client, apiBase, key, site string,
	actions []string, inputs map[string]string, mode string, limit int,
	sessionID string, keepSession bool,
) (string, error) {
	payload := map[string]any{
		"site":            site,
		"actions":         actions,
		"idempotency_key": newIdempotencyKey(),
	}
	if len(inputs) > 0 {
		payload["inputs"] = inputs
	}
	if strings.TrimSpace(mode) != "" {
		payload["mode"] = mode
	}
	// Only when the caller asked. 0 means "unset" server-side and keeps the
	// site's own cap, so sending it explicitly would be indistinguishable from
	// asking for zero records.
	if limit > 0 {
		payload["limit"] = limit
	}
	// Only when reusing. An empty session_id would be a request to reuse nothing,
	// which the server refuses rather than reading as "open a fresh one".
	if strings.TrimSpace(sessionID) != "" {
		payload["session_id"] = sessionID
	} else if keepSession {
		// Opening a session a later run will reuse. Without this the server closes
		// it when this run finishes and there is never a live session for a name to
		// point at -- which made --session silently open a fresh browser every
		// time.
		payload["keep_session"] = true
	}
	b, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiBase+"/v1/runtime/run", bytes.NewReader(b))
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
		return "", runAuthError(res.StatusCode, string(body))
	}
	var out runStartResponse
	lastJobBody = body
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("unreadable start response: %s", strings.TrimSpace(string(body)))
	}
	id := out.JobID
	if id == "" {
		id = out.ID
	}
	if id == "" {
		return "", fmt.Errorf("server did not return a job id: %s", strings.TrimSpace(string(body)))
	}
	return id, nil
}

func runJob(ctx context.Context, httpc *http.Client, apiBase, key, jobID string) (runJobEnvelope, error) {
	var out runJobEnvelope
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		apiBase+"/v1/runtime/jobs/"+url.PathEscape(jobID), nil)
	if err != nil {
		return out, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	res, err := httpc.Do(req)
	if err != nil {
		return out, err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if res.StatusCode != http.StatusOK {
		// A 404 HERE is an unknown JOB, not an unknown site. run's mapper answers
		// 404 with "map it first: rindler map <url>", which sends someone to map a
		// site that is mapped fine -- the job id is what is wrong, usually a typo
		// or a job that has aged out of the ledger.
		if res.StatusCode == http.StatusNotFound {
			return out, fmt.Errorf("no job with that id (check the id, or it may have aged out)")
		}
		return out, verbError("run status", res.StatusCode, string(body))
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return out, fmt.Errorf("unreadable job envelope: %s", strings.TrimSpace(string(body)))
	}
	return out, nil
}

// followRun polls a job to a terminal state.
//
// bindName, when set, is the session name waiting for an id. The id is NOT
// available at the 202: the server writes it when the run actually starts its
// browser (RunStarted), so a poll fired immediately after submission always sees
// nothing -- which is exactly how the first version of named sessions came to
// bind nothing at all and silently open a fresh browser every time. Binding here,
// on the first poll that carries one, is the earliest honest moment.
func followRun(
	ctx context.Context, httpc *http.Client, apiBase, key, jobID string, jsonOut bool, bindName string,
) int {
	bound := false
	last := ""
	for {
		env, err := runJob(ctx, httpc, apiBase, key, jobID)
		if err == nil && !bound && bindName != "" && env.SessionID != "" {
			bound = true
			if bindErr := bindSession(bindName, env.SessionID); bindErr != nil {
				// A failed bind costs a future reuse, not this run.
				fmt.Fprintf(os.Stderr, "warning: could not remember session %q: %v\n", bindName, bindErr)
			}
		}
		if err != nil {
			if ctx.Err() != nil {
				fmt.Fprintf(os.Stderr, "\nstopped following after the timeout; the run may still be going.\n"+
					"Check it with: rindler run status %s\n", jobID)
				return 1
			}
			fmt.Fprintln(os.Stderr, "run: status check failed:", err)
			return 1
		}
		if done, _ := runTerminal(env.Status); done {
			if jsonOut {
				fmt.Println(strings.TrimSpace(string(lastJobBody)))
			} else {
				printRunResult(os.Stdout, env)
			}
			return runExitCode(env)
		}
		if !jsonOut && env.Status != last {
			fmt.Println(" ", env.Status)
			last = env.Status
		}
		select {
		case <-ctx.Done():
			fmt.Fprintf(os.Stderr, "\nstopped following after the timeout; the run may still be going.\n"+
				"Check it with: rindler run status %s\n", jobID)
			return 1
		case <-time.After(runPollInterval):
		}
	}
}

// printRunResult renders the terminal envelope for a human, keeping the run
// status and the semantic retrieval outcome visibly distinct.
// lastJobBody holds the raw bytes of the most recent job poll, so `--json` can
// print the SERVER's response rather than a re-encode of our struct.
//
// A re-encode drops every field this CLI does not declare -- which is the same
// lossy `--json` already fixed for sites/actions, and the rule was stated there:
// a script asking for JSON must get the server's answer, not ours. Threading the
// body through every return would have meant changing four signatures for a
// debug flag; this keeps the change where the flag is read.
//
// Not concurrency-safe, and does not need to be: one run follows one job in one
// goroutine.
var lastJobBody []byte

func printRunResult(w io.Writer, env runJobEnvelope) {
	// done BEFORE ok. runTerminal returns (false,false) for a job still in
	// flight, so keying the marker on ok alone stamped a ✗ on every "queued"
	// and "running" -- a job that is merely not finished yet read as failed.
	done, ok := runTerminal(env.Status)
	switch {
	case !done:
		fmt.Fprintf(w, "\n· %s", env.Status)
	case ok:
		fmt.Fprintf(w, "\n✓ %s", env.Status)
	default:
		fmt.Fprintf(w, "\n✗ %s", env.Status)
	}
	if env.Usage.Steps > 0 {
		fmt.Fprintf(w, " (%d step(s))", env.Usage.Steps)
	}
	fmt.Fprintln(w)

	if msg := firstNonEmpty(env.ErrMsg, errViewMessage(env)); msg != "" {
		fmt.Fprintf(w, "  error: %s\n", msg)
	}

	n := 0
	if env.Outputs != nil {
		n = len(env.Outputs.Records)
	}
	if env.Outputs != nil && env.Outputs.Total > n {
		// "5 of 1200" is a different fact from "5", and the difference is the
		// whole reason to reach for --limit.
		fmt.Fprintf(w, "  records: %d of %d\n", n, env.Outputs.Total)
	} else {
		fmt.Fprintf(w, "  records: %d\n", n)
	}
	if env.Outputs != nil && env.Outputs.Truncated {
		fmt.Fprintln(w, "  note: the record set was truncated; raise it with --limit, or narrow the query")
	}
	// The link goes last, so it sits next to whatever went wrong above it.
	if env.Evidence != nil && env.Evidence.RunViewerURL != "" {
		fmt.Fprintf(w, "  look at it: %s\n", env.Evidence.RunViewerURL)
	}
	for i, rec := range recordsOf(env) {
		if i >= 20 {
			fmt.Fprintf(w, "  … %d more\n", n-20)
			break
		}
		fmt.Fprintf(w, "  - %s\n", summarizeRecord(rec))
	}

	// Retrieval is a SEPARATE verdict from status; print it whenever it is not a
	// clean answer, so "complete" never stands in for "found what you asked for".
	if r := env.Retrieval; r != nil && !r.Complete {
		fmt.Fprintf(w, "  retrieval: %s (not a complete answer)\n", r.Outcome)
		if r.FailureShape != "" {
			fmt.Fprintf(w, "    fault: %s\n", r.FailureShape)
		}
		for _, reason := range r.Reasons {
			fmt.Fprintf(w, "    reason: %s\n", reason)
		}
		if r.RetryGuidance != "" {
			fmt.Fprintf(w, "    next: %s\n", r.RetryGuidance)
		}
	}
}

func errViewMessage(env runJobEnvelope) string {
	if env.Error == nil {
		return ""
	}
	if env.Error.Class != "" && env.Error.Message != "" {
		return env.Error.Class + ": " + env.Error.Message
	}
	return firstNonEmpty(env.Error.Message, env.Error.Class)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func recordsOf(env runJobEnvelope) []map[string]any {
	if env.Outputs == nil {
		return nil
	}
	return env.Outputs.Records
}

// summarizeRecord renders one record on one line, preferring the fields a human
// scans for and falling back to sorted keys so output is deterministic.
func summarizeRecord(rec map[string]any) string {
	for _, k := range []string{"title", "name", "subject", "label", "id"} {
		if v, ok := rec[k]; ok && fmt.Sprint(v) != "" {
			extra := ""
			for _, pk := range []string{"price", "date", "status", "sender"} {
				if pv, ok := rec[pk]; ok && fmt.Sprint(pv) != "" {
					extra = fmt.Sprintf("  [%s: %v]", pk, pv)
					break
				}
			}
			return fmt.Sprintf("%v%s", v, extra)
		}
	}
	keys := make([]string, 0, len(rec))
	for k := range rec {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, 3)
	for i, k := range keys {
		if i >= 3 {
			break
		}
		parts = append(parts, fmt.Sprintf("%s=%v", k, rec[k]))
	}
	return strings.Join(parts, " ")
}

// runRunStatus follows a run started earlier by job id.
func runRunStatus(args []string) int {
	fs := flag.NewFlagSet("run status", flag.ContinueOnError)
	apiBaseFlag := fs.String("api-base", "", "Rindler API origin")
	timeout := fs.Duration("timeout", 15*time.Minute, "how long to follow the run")
	once := fs.Bool("once", false, "print the current state and exit instead of following")
	jsonOut := fs.Bool("json", false, "print the job envelope as JSON")
	rest, err := parseAnyOrder(fs, args)
	if err != nil {
		return 2
	}
	if len(rest) < 1 {
		fmt.Fprintln(os.Stderr, "usage: rindler run status <job-id> [--once] [--json]")
		return 2
	}
	jobID := rest[0]
	key, apiBase, code := resolveKeyAndBase(*apiBaseFlag, "run status")
	if code != 0 {
		return code
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	httpc := defaultHTTPClient()

	if *once {
		env, err := runJob(ctx, httpc, apiBase, key, jobID)
		if err != nil {
			fmt.Fprintln(os.Stderr, "run status:", err)
			return 1
		}
		if *jsonOut {
			fmt.Println(strings.TrimSpace(string(lastJobBody)))
		} else {
			printRunResult(os.Stdout, env)
		}
		// The SAME verdict the follow path uses. A shortcut that disagreed with
		// the thing it shortcuts is worse than no shortcut.
		return runExitCode(env)
	}
	return followRun(ctx, httpc, apiBase, key, jobID, *jsonOut, "")
}

// startRunInSession starts a run bound to a NAMED session, and returns the
// browser session it actually used so the name can be (re)bound to it.
//
// TRANSPARENT RE-ATTACH lives here. A bound id eventually dies: the idle reaper
// takes it, or the max lifetime does. When the server answers session_not_found
// for the id we sent, that is not a failure to report -- the caller asked for a
// session called `name`, not for one specific browser -- so we retry once
// WITHOUT the id and let a fresh session open under the same name.
//
// Retried ONCE, and only for that one class. A second failure is a real one, and
// retrying anything else would turn an unrelated error into two identical
// attempts.
func startRunInSession(
	ctx context.Context, httpc *http.Client, apiBase, key, site string,
	actions []string, inputs map[string]string, mode string, limit int, name string,
) (jobID string, sessionID string, err error) {
	bound := sessionIDFor(name)

	// keep_session ONLY when opening one. A run reusing a session must not ask to
	// extend its life; whoever opened it owns that.
	jobID, err = startRunWithSession(
		ctx, httpc, apiBase, key, site, actions, inputs, mode, limit, bound, bound == "")
	if err != nil && bound != "" && isSessionGone(err) {
		// The name outlives the browser. Drop the stale binding first, so a
		// failure after this point does not leave a known-dead id bound.
		_ = unbindSession(name)
		fmt.Fprintf(os.Stderr, "• session %q had expired; starting a fresh one\n", name)
		jobID, err = startRunWithSession(
			ctx, httpc, apiBase, key, site, actions, inputs, mode, limit, "", true)
	}
	if err != nil {
		return "", "", err
	}
	// Reusing? Then we already know the id, and the server just confirmed it by
	// accepting the run.
	if bound != "" {
		return jobID, bound, nil
	}
	return jobID, "", nil
}

// isSessionGone reports whether a failure means "that session is not available",
// the one class re-attach is allowed to retry.
func isSessionGone(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "session_not_found") ||
		strings.Contains(msg, "that session is not available")
}
