// `rindler doctor` — one command that says why the CLI is not working.
//
// The failure modes are all silent on their own: a key that expired with the
// Clerk session, a login that came back without mapping entitlement, an MCP
// installed into an agent that was never restarted, an API origin left pointing
// at a preview. Each shows up somewhere else as a confusing 401/403 later.
// Doctor checks them together and prints the fix, not the symptom.

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

type checkState int

const (
	checkOK checkState = iota
	checkWarn
	checkFail
)

type check struct {
	Name   string
	State  checkState
	Detail string
	Fix    string
}

func (c check) mark() string {
	switch c.State {
	case checkOK:
		return "✓"
	case checkWarn:
		return "!"
	default:
		return "✗"
	}
}

// runDoctor exits non-zero if any check FAILED, so it is usable in CI as a
// readiness gate. A warning alone does not fail the command.
func runDoctor(args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	apiBaseFlag := fs.String("api-base", "", "Rindler API origin")
	offline := fs.Bool("offline", false, "skip the live API check")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, _ := loadConfig()
	checks := diagnose(cfg, *apiBaseFlag, os.Getenv("RINDLER_API_KEY") != "", *offline)

	// The live leg. Local checks prove a key EXISTS; only this proves the server
	// still accepts it, which is the difference between "logged in" and "working".
	if !*offline {
		if key, apiBase, code := resolveKeyAndBaseQuiet(*apiBaseFlag); code == 0 && key != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			checks = append(checks, pingAPI(ctx, defaultHTTPClient(), apiBase, key))
			cancel()
		}
	}

	fmt.Println("rindler doctor")
	worst := checkOK
	for _, c := range checks {
		fmt.Printf("  %s %s", c.mark(), c.Name)
		if c.Detail != "" {
			fmt.Printf(" — %s", c.Detail)
		}
		fmt.Println()
		if c.Fix != "" && c.State != checkOK {
			fmt.Printf("      fix: %s\n", c.Fix)
		}
		if c.State > worst {
			worst = c.State
		}
	}
	if worst == checkFail {
		return 1
	}
	return 0
}

// diagnose is pure over its inputs plus the local agent config readers, so the
// verdict logic is testable without a network or a real keyring.
// offline tells diagnose whether the live probe will run. It changes one
// verdict: without a live leg the mint-time expiry snapshot is the only
// evidence there is, so it has to be the one that fails.
func diagnose(cfg cliConfig, apiBaseFlag string, envKey, offline bool) []check {
	var out []check

	// 1. Credential.
	switch {
	case envKey:
		out = append(out, check{Name: "login", State: checkOK,
			Detail: "using RINDLER_API_KEY from the environment"})
	default:
		store, warning, err := newCredentialStore()
		key := ""
		if err == nil && store != nil {
			key, _ = store.getKey()
		}
		if key == "" {
			out = append(out, check{Name: "login", State: checkFail,
				Detail: "not logged in", Fix: "rindler login"})
		} else {
			out = append(out, check{Name: "login", State: checkOK,
				Detail: fmt.Sprintf("key …%s", cfg.Last4)})
		}
		if warning != "" {
			out = append(out, check{Name: "keyring", State: checkWarn, Detail: warning,
				Fix: "install libsecret-tools (Linux) so the key is not stored in a plain file"})
		}
	}

	// 2. Expiry. A CLI key dies with the Clerk session, so "logged in" is not the
	// same as "still valid" — this is the single most confusing 401 there is.
	if !envKey && cfg.ExpiresAt != "" {
		if msg, expired := cfg.expiryStatus(time.Now()); msg != "" {
			// A WARNING even when the local clock says expired, because this
			// timestamp is a snapshot taken at mint time and the server can have
			// invalidated it in either direction since: a revoked key still looks
			// valid here, and a refreshed session still looks dead. The live probe
			// below asks the only authority there is, so let IT fail the run and
			// keep this as the hint that explains why.
			//
			// Failing here instead sent people to `rindler login` over a stale
			// local file while their key worked perfectly.
			state := checkWarn
			if expired && offline {
				// With no live leg this snapshot is all we have, so it has to
				// carry the verdict.
				state = checkFail
			}
			out = append(out, check{Name: "key expiry", State: state, Detail: msg, Fix: "rindler login"})
		} else {
			out = append(out, check{Name: "key expiry", State: checkOK, Detail: "expires " + cfg.ExpiresAt})
		}
	}

	// 3. Mapping entitlement — a warning, not a failure: everything except

	// 4. API origin. Shared resolver: this was a FOURTH hand-rolled copy of the
	// ladder that skipped RINDLER_API_BASE -- while its own Fix line told the
	// reader to unset that variable, which could not have been the cause because
	// nothing here ever read it.
	base := resolveAPIBase(apiBaseFlag, cfg)
	st := checkOK
	detail := base
	if base != defaultAPIBase {
		st = checkWarn
		detail = base + " (not the default)"
	}
	out = append(out, check{Name: "api origin", State: st, Detail: detail,
		Fix: "unset RINDLER_API_BASE, or log in again, to return to " + defaultAPIBase})

	return out
}

// pingAPI is the optional live leg: it proves the stored key still authenticates
// rather than merely existing. Kept separate from diagnose so the pure checks
// stay testable offline.
func pingAPI(ctx context.Context, httpc *http.Client, apiBase, key string) check {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBase+"/v1/runtime/configs", nil)
	if err != nil {
		return check{Name: "api reachable", State: checkFail, Detail: err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+key)
	res, err := httpc.Do(req)
	if err != nil {
		return check{Name: "api reachable", State: checkFail, Detail: err.Error(),
			Fix: "check your network, or --api-base"}
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<16))
	switch res.StatusCode {
	case http.StatusOK:
		var resp configsResponse
		_ = json.Unmarshal(body, &resp)
		return check{Name: "api reachable", State: checkOK,
			// "your own" because this endpoint is owner-fenced: it lists the
			// configs YOU published, not the workspace's shared sites and not the
			// platform catalog. "N sites available" read as a total, so a healthy
			// account with plenty of shared sites looked nearly empty.
			Detail: fmt.Sprintf("reachable, %d site(s) you have mapped", len(resp.Configs))}
	case http.StatusUnauthorized:
		return check{Name: "api reachable", State: checkFail,
			Detail: "the key is not accepted (expired or revoked)", Fix: "rindler login"}
	default:
		return check{Name: "api reachable", State: checkFail,
			Detail: fmt.Sprintf("HTTP %d: %s", res.StatusCode, strings.TrimSpace(string(body)))}
	}
}

// resolveKeyAndBaseQuiet is resolveKeyAndBase without the stderr chatter: doctor
// already reports "not logged in" as its own check and must not print it twice.
func resolveKeyAndBaseQuiet(apiBaseFlag string) (key, apiBase string, exitCode int) {
	cfg, _ := loadConfig()
	store, _, err := newCredentialStore()
	if err != nil || store == nil {
		return "", "", 1
	}
	key, _, err = resolveActiveKey(store)
	if err != nil || key == "" {
		return "", "", 1
	}
	// Same resolver as every other command, on purpose: a doctor that diagnosed
	// a different origin than the one your commands use would confidently
	// describe a lane you are not talking to.
	return key, resolveAPIBase(apiBaseFlag, cfg), 0
}
