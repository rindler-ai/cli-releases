// `rindler run <site> "<task>"` — say what you want done, in your own words.
//
// This is the CLI's primary verb. It sends the sentence to the dashboard's
// build-and-run endpoint, which turns it into an automation and runs it once,
// and prints the one answer that comes back.
//
// WHY THE DASHBOARD ORIGIN AND NOT THE API ORIGIN. The thing that reads a
// sentence lives beside the dashboard, and the run engine lives behind the API
// origin; this verb needs the first, which then calls the second with the same
// key. `rindler login` already talks to both origins, so this is not a new
// relationship, and RINDLER_AUTHORIZE_BASE overrides it exactly as it does for
// the consent page.
//
// ONE CALL, ONE ANSWER. There is no job id to poll and no second "now run it"
// step: the build's trial run IS the run. That is why the timeout here is
// minutes rather than seconds.

package main

import (
	"bytes"
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

// taskDeadline bounds one build-and-run. A first pass opens a real browser and
// drives a real site, so this is sized for the slow end of a working run
// rather than the median: giving up early on a run that was going to succeed
// costs the customer the whole thing.
const taskDeadline = 6 * time.Minute

type taskRequest struct {
	Site string `json:"site"`
	Task string `json:"task"`
}

// taskResponse is every field the endpoint can send. `Outcome` is the
// discriminator and is the ONLY thing this client branches on — never the HTTP
// status, so a proxy that rewrites a status cannot change what the customer is
// told.
type taskResponse struct {
	Outcome  string   `json:"outcome"`
	Site     string   `json:"site,omitempty"`
	Name     string   `json:"name,omitempty"`
	Summary  string   `json:"summary,omitempty"`
	Message  string   `json:"message,omitempty"`
	Question string   `json:"question,omitempty"`
	Actions  []string `json:"actions,omitempty"`
	Schedule *struct {
		State  string `json:"state"`
		Reason string `json:"reason,omitempty"`
	} `json:"schedule,omitempty"`
	NeedsApproval  bool `json:"needs_approval,omitempty"`
	RetryAfterSecs int  `json:"retry_after_seconds,omitempty"`
}

// runTask is the `rindler run <site> "<task>"` path.
//
// Exit codes carry the outcome so a script can branch without parsing prose:
// 0 built, 1 we failed, 3 the site cannot, 4 it needs a confirmation the CLI
// cannot collect.
func runTask(site, task string, args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	apiBaseFlag := fs.String("api-base", "", "Rindler API origin (defaults to the one you logged in against)")
	jsonOut := fs.Bool("json", false, "print the raw answer as JSON")
	timeout := fs.Duration("timeout", taskDeadline, "how long to wait for the run")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	host, err := siteFromTarget(site)
	if err != nil {
		fmt.Fprintln(os.Stderr, "run:", err)
		return 2
	}
	if strings.TrimSpace(task) == "" {
		fmt.Fprintln(os.Stderr, `usage: rindler run <site> "<what you want done>"`)
		return 2
	}

	key, _, code := resolveKeyAndBase(*apiBaseFlag, "run")
	if code != 0 {
		return code
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	// Said BEFORE the call, not after: a first pass takes a minute or more, and
	// a silent terminal for that long reads as a hang.
	if !*jsonOut {
		fmt.Printf("Working on %s…\n", host)
	}

	answer, raw, err := postTask(ctx, key, taskRequest{Site: host, Task: task})
	if err != nil {
		fmt.Fprintln(os.Stderr, "run:", err)
		return 1
	}
	if *jsonOut {
		fmt.Println(strings.TrimSpace(string(raw)))
		return exitForOutcome(answer.Outcome)
	}
	printTaskAnswer(answer)
	return exitForOutcome(answer.Outcome)
}

func postTask(ctx context.Context, key string, body taskRequest) (taskResponse, []byte, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return taskResponse{}, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(envOr("RINDLER_AUTHORIZE_BASE", defaultAuthorizeBase), "/")+"/api/cli/tasks",
		bytes.NewReader(payload))
	if err != nil {
		return taskResponse{}, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)

	res, err := defaultHTTPClient().Do(req)
	if err != nil {
		// A deadline here is the RUN being slow, not the server being broken,
		// and saying so keeps someone from re-running a task that may well have
		// completed on the far side.
		if ctx.Err() != nil {
			return taskResponse{}, nil, fmt.Errorf(
				"the run was still going after %s; check your automations on the dashboard before running it again",
				taskDeadline)
		}
		return taskResponse{}, nil, err
	}
	defer func() { _ = res.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return taskResponse{}, nil, err
	}
	var answer taskResponse
	if err := json.Unmarshal(raw, &answer); err != nil || answer.Outcome == "" {
		// An unparseable body is OUR failure, and it must not be printed raw at
		// the customer: an HTML error page or a proxy notice is not an answer
		// about their site.
		return taskResponse{}, raw, fmt.Errorf("the server sent an answer this version could not read (HTTP %d)", res.StatusCode)
	}
	return answer, raw, nil
}

// printTaskAnswer says what happened in the customer's terms. No action ids, no
// chain vocabulary, no mention of a config: those are ours, not theirs.
func printTaskAnswer(a taskResponse) { printTaskAnswerTo(os.Stdout, os.Stderr, a) }

// printTaskAnswerTo is the testable form. Two writers rather than one because
// the split is real -- a success goes to stdout so it can be piped, a refusal
// to stderr so it does not -- and a test that could not see both would not be
// able to check the copy on the branches that matter most.
func printTaskAnswerTo(out, errOut io.Writer, a taskResponse) {
	switch a.Outcome {
	case "built":
		fmt.Fprintf(out, "✓ %s\n", firstNonEmpty(a.Summary, a.Name, "Done."))
		if a.Schedule != nil && a.Schedule.State == "not_armed" && a.Schedule.Reason != "" {
			fmt.Fprintf(out, "  %s\n", a.Schedule.Reason)
		}
		if a.NeedsApproval {
			fmt.Fprintln(out, "  One step needs your approval before it runs unattended.")
		}
		fmt.Fprintf(out, "  Saved as \"%s\" — run it again any time.\n", a.Name)
	case "nearly":
		fmt.Fprintf(out, "Almost: %s\n", firstNonEmpty(a.Summary, a.Name))
		if a.Question != "" {
			fmt.Fprintf(out, "  It needs one thing answered first: %s\n", a.Question)
		}
		fmt.Fprintln(out, "  Finish it on your dashboard, under Automations.")
	case "cannot":
		fmt.Fprintf(errOut, "✗ %s\n", firstNonEmpty(a.Message, "That is not something this site can do."))
	case "unauthorized":
		fmt.Fprintf(errOut, "✗ %s\n", firstNonEmpty(a.Message, "Run `rindler login` first."))
	case "blocked":
		fmt.Fprintf(errOut, "✗ %s\n", firstNonEmpty(a.Message, "That request could not be started."))
		if a.RetryAfterSecs > 0 {
			fmt.Fprintf(errOut, "  Try again in about %d seconds.\n", a.RetryAfterSecs)
		}
	default: // "unavailable" and anything a newer server adds
		fmt.Fprintf(errOut, "✗ %s\n", firstNonEmpty(a.Message, "Something on our side went wrong. Try again in a moment."))
	}
}

// exitForOutcome keys the exit code on the OUTCOME, so "the site cannot do
// this" (3) and "we failed, retry" (1) are distinguishable by a script that
// reads neither stdout nor the HTTP status.
func exitForOutcome(outcome string) int {
	switch outcome {
	case "built":
		return 0
	case "cannot":
		return 3
	case "nearly":
		return 4
	default:
		return 1
	}
}
