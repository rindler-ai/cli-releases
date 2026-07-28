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
// The action surface is served REDACTED: action_name
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
// {"configs":[...]}, not a bare array. Decoding it as an array
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
// getJSON reads a JSON endpoint. verb names the COMMAND for error messages:
// it used to hardcode `run`'s mapper, so a failed `rindler sites` announced
// "run failed" and offered run's advice about a site the user never named.
func getJSON(ctx context.Context, httpc *http.Client, apiBase, key, verb, path string, out any) error {
	_, err := getJSONRaw(ctx, httpc, apiBase, key, verb, path, out)
	return err
}

// getJSONRaw is getJSON plus the response bytes, for `--json` to print
// VERBATIM.
//
// Re-encoding the decoded struct instead -- which is what --json did -- is a
// promise this CLI cannot keep: any field the struct does not declare is dropped
// silently, so a script consuming --json sees the CLI's idea of the response
// rather than the server's. That is the same under-transcription that zeroed
// `usage`, except here it is the user's data, not ours.
func getJSONRaw(
	ctx context.Context, httpc *http.Client, apiBase, key, verb, path string, out any,
) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBase+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	res, err := httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if res.StatusCode != http.StatusOK {
		return nil, verbError(verb, res.StatusCode, string(body))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return nil, fmt.Errorf("unreadable response from %s: %s", path, strings.TrimSpace(string(body)))
	}
	return body, nil
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
	raw, err := getJSONRaw(ctx, defaultHTTPClient(), apiBase, key, "sites", "/v1/runtime/configs", &resp)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sites:", err)
		return 1
	}
	sites := resp.Configs
	if *jsonOut {
		// The server's bytes, not our re-encode of them. A field this CLI does
		// not declare must still reach a script that asked for JSON.
		fmt.Println(strings.TrimSpace(string(raw)))
		return 0
	}
	if len(sites) == 0 {
		// Say precisely what is empty. This endpoint lists the configs YOU have
		// published, not the workspace's shared sites and not the platform
		// catalog, so "no sites available" was a claim about the account that
		// this response cannot support -- and it reads as "Rindler has nothing
		// for you" to someone whose workspace has plenty.
		fmt.Println("You have not mapped any sites yet.")
		fmt.Println("This lists your own mapped sites; sites shared with your")
		fmt.Println("workspace are on the dashboard.")
		fmt.Println("\nMap one:  rindler map https://example.com")
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
	byScreen := fs.Bool("by-screen", false, "group actions by screen instead of deduplicating them")
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
	raw, err := getJSONRaw(ctx, defaultHTTPClient(), apiBase, key, "actions",
		"/v1/runtime/configs/"+url.PathEscape(host), &detail)
	if err != nil {
		fmt.Fprintln(os.Stderr, "actions:", err)
		return 1
	}
	if *jsonOut {
		// The server's bytes, verbatim. See getJSONRaw.
		fmt.Println(strings.TrimSpace(string(raw)))
		return 0
	}
	printActions(os.Stdout, detail, *all, *byScreen)
	return 0
}

// printActions renders the action surface as the thing a user copies into a
// `run` invocation: the action_name, whether it reads or acts, and its inputs.
//
// DEDUPED BY ACTION NAME by default. A config lists each action under every
// screen it is reachable from, so the Gmail map -- 11 distinct actions across 5
// screens -- rendered 31 rows, with view_inbox appearing five times. That is a
// transcript, not a menu, and `run` takes the action NAME anyway: which screen it
// was found under changes nothing about how you invoke it. --by-screen restores
// the grouped view for anyone who wants the topology.
func printActions(out io.Writer, detail siteDetail, includeDisabled, byScreen bool) {
	fmt.Fprintf(out, "%s (v%d)", detail.Domain, detail.Version)
	if detail.Authed {
		fmt.Fprint(out, "  [needs login]")
	}
	fmt.Fprintln(out)

	if byScreen {
		printActionsByScreen(out, detail, includeDisabled)
		return
	}

	type entry struct {
		a       projAction
		screens []string
	}
	var order []string
	seen := map[string]*entry{}
	for _, sc := range detail.Screens {
		for _, a := range sc.Actions {
			if !a.Enabled && !includeDisabled {
				continue
			}
			e, ok := seen[a.ActionName]
			if !ok {
				e = &entry{a: a}
				seen[a.ActionName] = e
				order = append(order, a.ActionName)
			}
			if sc.Name != "" {
				e.screens = append(e.screens, sc.Name)
			}
		}
	}
	if len(order) == 0 {
		fmt.Fprintln(out, "\n  No enabled actions. Try --all to include disabled ones.")
		return
	}
	// Reads first, then writes: a reader is the safe thing to try, and grouping
	// them separates "look at the site" from "change something on it".
	sort.SliceStable(order, func(i, j int) bool {
		li, lj := seen[order[i]].a.Method != "act", seen[order[j]].a.Method != "act"
		if li != lj {
			return li
		}
		return order[i] < order[j]
	})

	lastKind := ""
	for _, name := range order {
		e := seen[name]
		kind := e.a.Method
		if kind == "" {
			kind = "read"
		}
		if kind != lastKind {
			fmt.Fprintf(out, "\n  %s\n", map[string]string{"read": "reads", "act": "writes"}[kind])
			lastKind = kind
		}
		line := fmt.Sprintf("    %-24s", name)
		if !e.a.Enabled {
			line += " (disabled)"
		}
		fmt.Fprintln(out, line)
		if len(e.a.Params) > 0 {
			parts := make([]string, 0, len(e.a.Params))
			for _, p := range e.a.Params {
				s := "--input " + p.Name + "=…"
				if p.Required {
					s += "*"
				}
				parts = append(parts, s)
			}
			fmt.Fprintf(out, "      %s\n", strings.Join(parts, "  "))
		}
		if d := firstSentence(e.a.Description); d != "" {
			fmt.Fprintf(out, "      %s\n", d)
		}
	}
	fmt.Fprintf(out, "\n%d action(s); * = required. Grouped by screen: --by-screen\n", len(order))
	fmt.Fprintf(out, "Run one:  rindler run --site %s --action <name>\n", detail.Domain)
}

// printActionsByScreen is the original topology view, behind --by-screen.
func printActionsByScreen(out io.Writer, detail siteDetail, includeDisabled bool) {
	shown := 0
	for _, sc := range detail.Screens {
		var rows []projAction
		for _, a := range sc.Actions {
			if a.Enabled || includeDisabled {
				rows = append(rows, a)
			}
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
			line := fmt.Sprintf("    %-24s %s", a.ActionName, kind)
			if !a.Enabled {
				line += " (disabled)"
			}
			fmt.Fprintln(out, line)
		}
	}
	if shown == 0 {
		fmt.Fprintln(out, "\n  No enabled actions. Try --all to include disabled ones.")
		return
	}
	fmt.Fprintf(out, "\nRun one:  rindler run --site %s --action <name>\n", detail.Domain)
}

// firstSentence trims a description to its first sentence, capped. Action
// descriptions are written for an agent and run long; a discovery listing needs
// one scannable line, not a paragraph per row.
func firstSentence(d string) string {
	d = strings.TrimSpace(strings.ReplaceAll(d, "\n", " "))
	if d == "" {
		return ""
	}
	if i := strings.Index(d, ". "); i > 0 {
		d = d[:i+1]
	}
	const cap = 96
	if len(d) > cap {
		d = strings.TrimSpace(d[:cap]) + "…"
	}
	return d
}
