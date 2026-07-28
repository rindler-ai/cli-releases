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

type retrievalView struct {
	Outcome       string   `json:"outcome"`
	Complete      bool     `json:"complete"`
	Reasons       []string `json:"reasons,omitempty"`
	RetryGuidance string   `json:"retry_guidance,omitempty"`
	FailureShape  string   `json:"failure_shape,omitempty"`
}

type runJobEnvelope struct {
	JobID   string `json:"job_id"`
	ID      string `json:"id"`
	Status  string `json:"status"`
	Verb    string `json:"verb,omitempty"`
	Site    string `json:"site,omitempty"`
	ErrMsg  string `json:"error_msg,omitempty"`
	Outputs *struct {
		Records   []map[string]any `json:"records"`
		Truncated bool             `json:"truncated,omitempty"`
	} `json:"outputs,omitempty"`
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

// runTerminal classifies a job status. An UNRECOGNISED status is deliberately
// non-terminal: treating an unknown value as done would end the poll early and
// report whatever partial state happened to be there as the answer.
// runTerminal decides when to stop polling a run. Same contract, and same
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

	jobID, err := startRun(ctx, httpc, apiBase, key, host, actions, inputMap, *mode)
	if err != nil {
		fmt.Fprintln(os.Stderr, "run:", err)
		return 1
	}
	if !*jsonOut {
		fmt.Printf("Running %s on %s…\njob %s\n", strings.Join(actions, ", "), host, jobID)
	}
	if *noWait {
		fmt.Printf("Follow it with: rindler run status %s\n", jobID)
		return 0
	}
	return followRun(ctx, httpc, apiBase, key, jobID, *jsonOut)
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
	apiBase = apiBaseFlag
	if apiBase == "" {
		apiBase = cfg.APIBase
	}
	if apiBase == "" {
		apiBase = defaultAPIBase
	}
	return key, strings.TrimRight(apiBase, "/"), 0
}

// runAuthError names what each refusal means. Run accepts any key, so a 403 here
// is about the SITE, not the key — pointing the user at `rindler login` would
// send them to fix something that is not broken.
func runAuthError(code int, body string) error {
	switch code {
	case http.StatusUnauthorized:
		return fmt.Errorf("not logged in or the key expired — run `rindler login`")
	case http.StatusForbidden:
		return fmt.Errorf("not allowed to run against this site — it is not in your catalog " +
			"and you do not own a config for it (run accepts any key, so this is about the site, not your login)")
	case http.StatusNotFound:
		return fmt.Errorf("no config for that site — map it first: rindler map <url>")
	case http.StatusTooManyRequests:
		return fmt.Errorf("rate limited or out of quota; try again shortly")
	}
	return fmt.Errorf("run failed (HTTP %d): %s", code, strings.TrimSpace(body))
}

func startRun(
	ctx context.Context, httpc *http.Client, apiBase, key, site string,
	actions []string, inputs map[string]string, mode string,
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
		return out, runAuthError(res.StatusCode, string(body))
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return out, fmt.Errorf("unreadable job envelope: %s", strings.TrimSpace(string(body)))
	}
	return out, nil
}

func followRun(ctx context.Context, httpc *http.Client, apiBase, key, jobID string, jsonOut bool) int {
	last := ""
	for {
		env, err := runJob(ctx, httpc, apiBase, key, jobID)
		if err != nil {
			if ctx.Err() != nil {
				fmt.Fprintf(os.Stderr, "\nstopped following after the timeout; the run may still be going.\n"+
					"Check it with: rindler run status %s\n", jobID)
				return 1
			}
			fmt.Fprintln(os.Stderr, "run: status check failed:", err)
			return 1
		}
		if done, ok := runTerminal(env.Status); done {
			if jsonOut {
				b, _ := json.MarshalIndent(env, "", "  ")
				fmt.Println(string(b))
			} else {
				printRunResult(os.Stdout, env)
			}
			// The semantic outcome decides the exit code when it is present: a job
			// that RAN fine but retrieved nothing usable is not a success to a
			// script, which is the whole reason the server reports both.
			if !ok {
				return 1
			}
			if env.Retrieval != nil && !env.Retrieval.Complete {
				return 1
			}
			return 0
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
func printRunResult(w io.Writer, env runJobEnvelope) {
	if _, ok := runTerminal(env.Status); ok {
		fmt.Fprintf(w, "\n✓ %s", env.Status)
	} else {
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
	fmt.Fprintf(w, "  records: %d\n", n)
	if env.Outputs != nil && env.Outputs.Truncated {
		fmt.Fprintln(w, "  note: the record set was truncated (raise --limit server-side or narrow the query)")
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
			b, _ := json.MarshalIndent(env, "", "  ")
			fmt.Println(string(b))
		} else {
			printRunResult(os.Stdout, env)
		}
		if done, ok := runTerminal(env.Status); done && !ok {
			return 1
		}
		return 0
	}
	return followRun(ctx, httpc, apiBase, key, jobID, *jsonOut)
}
