// `rindler sites` and `rindler actions <site>` — discovery.
//
// This is the load-bearing pair for anyone who has never used the web app. Every
// other verb takes names you cannot guess: `run --site X --action Y` is unusable
// unless something tells you which sites you can act on and what each one
// exposes. Without these two commands the CLI is only operable by someone who
// already learned the vocabulary somewhere else.
//
// Wire contract:
//   GET <api>/v1/runtime/configs           -> {configs:[{domain, version, authed, action_count}]}
//   GET <api>/v1/runtime/configs/{domain}  -> {domain, version, screens:[{name, actions:[…]}]}
//
// The action surface is served REDACTED (the server): action_name
// is what `run --action` wants, `method` is read vs act, and `params` are the
// bindable `--input` keys.

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

// configsResponse is the REAL envelope: GET /v1/runtime/configs returns
// {"configs":[...]}, not a bare array (the server
// Decoding it as an array
// silently yielded zero sites against the live server while the unit tests --
// written against the assumed shape -- stayed green. Caught by a live run.
type configsResponse struct {
	Configs []siteSummary `json:"configs"`
}

type siteSummary struct {
	Domain      string `json:"domain"`
	Version     int32  `json:"version"`
	UpdatedAt   string `json:"updated_at,omitempty"`
	Authed      bool   `json:"authed"`
	ExpiresAt   string `json:"expires_at,omitempty"`
	ActionCount *int   `json:"action_count,omitempty"`
}

type projParam struct {
	Name     string `json:"name"`
	Required bool   `json:"required"`
}

type projAction struct {
	ActionName  string      `json:"action_name"`
	Name        string      `json:"name"`
	Method      string      `json:"method"`
	Enabled     bool        `json:"enabled"`
	Params      []projParam `json:"params,omitempty"`
	Description string      `json:"description"`
	Global      bool        `json:"global,omitempty"`
}

type projScreen struct {
	Name        string       `json:"name"`
	Description string       `json:"description"`
	Actions     []projAction `json:"actions"`
}

type siteDetail struct {
	Domain  string       `json:"domain"`
	Version int32        `json:"version"`
	Authed  bool         `json:"authed"`
	Screens []projScreen `json:"screens"`
}

// getJSON is the shared authenticated GET for the read-only discovery verbs.
func getJSON(ctx context.Context, httpc *http.Client, apiBase, key, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBase+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	res, err := httpc.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if res.StatusCode != http.StatusOK {
		return runAuthError(res.StatusCode, string(body))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("unreadable response from %s: %s", path, strings.TrimSpace(string(body)))
	}
	return nil
}

func runSites(args []string) int {
	fs := flag.NewFlagSet("sites", flag.ContinueOnError)
	apiBaseFlag := fs.String("api-base", "", "Rindler API origin")
	jsonOut := fs.Bool("json", false, "print the raw JSON list")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	key, apiBase, code := resolveKeyAndBase(*apiBaseFlag, "sites")
	if code != 0 {
		return code
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var resp configsResponse
	if err := getJSON(ctx, defaultHTTPClient(), apiBase, key, "/v1/runtime/configs", &resp); err != nil {
		fmt.Fprintln(os.Stderr, "sites:", err)
		return 1
	}
	sites := resp.Configs
	if *jsonOut {
		b, _ := json.MarshalIndent(sites, "", "  ")
		fmt.Println(string(b))
		return 0
	}
	if len(sites) == 0 {
		// An empty catalog is the state a brand-new account is IN, so say what to
		// do next rather than printing nothing and looking broken.
		fmt.Println("No sites available yet.")
		fmt.Println("Map one to get started:  rindler map https://example.com")
		return 0
	}
	sort.Slice(sites, func(i, j int) bool { return sites[i].Domain < sites[j].Domain })
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SITE\tACTIONS\tLOGIN\tVERSION")
	for _, s := range sites {
		actions := "?"
		if s.ActionCount != nil {
			actions = fmt.Sprint(*s.ActionCount)
		}
		login := "no"
		if s.Authed {
			login = "yes"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\tv%d\n", s.Domain, actions, login, s.Version)
	}
	_ = w.Flush()
	fmt.Printf("\n%d site(s). See what one can do:  rindler actions <site>\n", len(sites))
	return 0
}

func runActions(args []string) int {
	fs := flag.NewFlagSet("actions", flag.ContinueOnError)
	apiBaseFlag := fs.String("api-base", "", "Rindler API origin")
	jsonOut := fs.Bool("json", false, "print the raw JSON detail")
	all := fs.Bool("all", false, "include actions that are currently disabled")
	rest, err := parseAnyOrder(fs, args)
	if err != nil {
		return 2
	}
	if len(rest) < 1 {
		fmt.Fprintln(os.Stderr, "usage: rindler actions <site> [--all] [--json]")
		return 2
	}
	host, err := siteFromTarget(rest[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "actions:", err)
		return 2
	}
	key, apiBase, code := resolveKeyAndBase(*apiBaseFlag, "actions")
	if code != 0 {
		return code
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var detail siteDetail
	if err := getJSON(ctx, defaultHTTPClient(), apiBase, key,
		"/v1/runtime/configs/"+url.PathEscape(host), &detail); err != nil {
		fmt.Fprintln(os.Stderr, "actions:", err)
		return 1
	}
	if *jsonOut {
		b, _ := json.MarshalIndent(detail, "", "  ")
		fmt.Println(string(b))
		return 0
	}
	printActions(os.Stdout, detail, *all)
	return 0
}

// printActions renders the action surface as the thing a user copies into a
// `run` invocation: the action_name, whether it reads or acts, and its inputs.
func printActions(out io.Writer, detail siteDetail, includeDisabled bool) {
	fmt.Fprintf(out, "%s (v%d)", detail.Domain, detail.Version)
	if detail.Authed {
		fmt.Fprint(out, "  [needs login]")
	}
	fmt.Fprintln(out)

	shown := 0
	seen := map[string]bool{}
	for _, sc := range detail.Screens {
		var rows []projAction
		for _, a := range sc.Actions {
			if !a.Enabled && !includeDisabled {
				continue
			}
			// A global action repeats on every screen; listing it once keeps the
			// output a menu rather than a transcript.
			if a.Global {
				if seen[a.ActionName] {
					continue
				}
				seen[a.ActionName] = true
			}
			rows = append(rows, a)
		}
		if len(rows) == 0 {
			continue
		}
		label := sc.Name
		if label == "" {
			label = "(screen)"
		}
		fmt.Fprintf(out, "\n  %s\n", label)
		for _, a := range rows {
			shown++
			kind := a.Method
			if kind == "" {
				kind = "read"
			}
			line := fmt.Sprintf("    %-26s %s", a.ActionName, kind)
			if !a.Enabled {
				line += " (disabled)"
			}
			fmt.Fprintln(out, line)
			if len(a.Params) > 0 {
				parts := make([]string, 0, len(a.Params))
				for _, p := range a.Params {
					s := "--input " + p.Name + "=…"
					if p.Required {
						s += " (required)"
					}
					parts = append(parts, s)
				}
				fmt.Fprintf(out, "      %s\n", strings.Join(parts, "  "))
			}
			if a.Description != "" {
				fmt.Fprintf(out, "      %s\n", a.Description)
			}
		}
	}
	if shown == 0 {
		fmt.Fprintln(out, "\n  No enabled actions. Try --all to include disabled ones.")
		return
	}
	fmt.Fprintf(out, "\nRun one:  rindler run --site %s --action <name>\n", detail.Domain)
}
