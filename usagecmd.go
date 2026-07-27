package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

// `rindler usage` — your own usage and credits, read from the same endpoint and
// under the same rules as the dashboard's Usage tab.
//
// It deliberately mirrors rather than re-derives. The dashboard already settled
// the hard parts (one grouped query, an optional personal filter, a refused
// unknown scope, a client that reads the scope back), and a second
// implementation would produce a second answer to the same question. The one
// nobody was looking at would be the wrong one.

const (
	scopeMe        = "me"
	scopeWorkspace = "workspace"

	// The disclosures the dashboard shows. They are true of the DATA, not of the
	// surface, so the CLI owes the reader the same two sentences.
	creditsReconstructedNote = "Per-member credits are reconstructed from telemetry, not read from a per-member ledger."
	alsoVisibleNote          = "Owners and admins can see these same numbers."
)

type usageResponse struct {
	// Scope is the server's statement of what it actually answered. Read back,
	// never assumed: rendering workspace totals under a personal heading is a
	// worse failure than printing nothing.
	Scope   string `json:"scope"`
	Credits struct {
		Remaining int `json:"remaining"`
		Used      int `json:"used"`
		Allotment int `json:"allotment"`
	} `json:"credits"`
	CreditsReconstructed bool `json:"credits_reconstructed"`
	Sessions             struct {
		Total    int `json:"total"`
		Refunded int `json:"refunded"`
	} `json:"sessions"`
	Sites []struct {
		Domain       string `json:"domain"`
		Sessions     int    `json:"sessions"`
		Credits      int    `json:"credits"`
		LastWorkedAt string `json:"last_worked_at"`
	} `json:"sites"`
}

func runUsage(args []string) int {
	fs := flag.NewFlagSet("usage", flag.ContinueOnError)
	apiBaseFlag := fs.String("api-base", "", "Rindler API origin")
	workspace := fs.Bool("workspace", false, "show the whole workspace instead of just you")
	jsonOut := fs.Bool("json", false, "print the raw JSON")
	if _, err := parseAnyOrder(fs, args); err != nil {
		return 2
	}
	key, apiBase, code := resolveKeyAndBase(*apiBaseFlag, "usage")
	if code != 0 {
		return code
	}

	// Only these two literals ever go over the wire. The CLI never accepts a
	// member id: which member you are is resolved from the verified key
	// server-side, so there is nothing here to widen.
	want := scopeMe
	if *workspace {
		want = scopeWorkspace
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(apiBase, "/")+"/api/workspace/usage/me?member="+want, nil)
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

	// READ THE SCOPE BACK. If the server answered a different question than the
	// one asked, printing its numbers under our heading would be a quiet lie, so
	// this is an error rather than a render.
	if u.Scope != "" && u.Scope != want {
		fmt.Fprintf(os.Stderr,
			"usage: asked for %q but the server answered %q; refusing to print numbers under the wrong heading\n",
			want, u.Scope)
		return 1
	}

	printUsage(os.Stdout, u, want)
	return 0
}

func printUsage(w io.Writer, u usageResponse, scope string) {
	heading := "Your usage"
	if scope == scopeWorkspace {
		heading = "Workspace usage"
	}
	fmt.Fprintf(w, "%s\n\n", heading)

	if u.Credits.Allotment > 0 {
		fmt.Fprintf(w, "  credits  %s  %d of %d remaining\n",
			creditBar(u.Credits.Remaining, u.Credits.Allotment),
			u.Credits.Remaining, u.Credits.Allotment)
	} else {
		fmt.Fprintf(w, "  credits  %d used\n", u.Credits.Used)
	}

	if u.Sessions.Total > 0 {
		// A refunded session is one that charged and then gave the credit back
		// because nothing succeeded, so it is the honest denominator for "did my
		// runs work", not a footnote.
		fmt.Fprintf(w, "  sessions %d, of which %d refunded\n",
			u.Sessions.Total, u.Sessions.Refunded)
	}

	if len(u.Sites) > 0 {
		fmt.Fprintln(w)
		sites := append([]struct {
			Domain       string `json:"domain"`
			Sessions     int    `json:"sessions"`
			Credits      int    `json:"credits"`
			LastWorkedAt string `json:"last_worked_at"`
		}(nil), u.Sites...)
		sort.Slice(sites, func(i, j int) bool { return sites[i].Sessions > sites[j].Sessions })
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "SITE\tSESSIONS\tCREDITS\tLAST WORKED")
		for _, s := range sites {
			fmt.Fprintf(tw, "%s\t%d\t%d\t%s\n", s.Domain, s.Sessions, s.Credits, s.LastWorkedAt)
		}
		tw.Flush()
	}

	fmt.Fprintln(w)
	if u.CreditsReconstructed {
		fmt.Fprintf(w, "  %s\n", creditsReconstructedNote)
	}
	fmt.Fprintf(w, "  %s\n", alsoVisibleNote)
}

// creditBar renders remaining/total as a bar. Clamped at both ends so an
// over-spend or a bad total cannot draw a negative or runaway bar.
func creditBar(remaining, total int) string {
	const width = 20
	if total <= 0 {
		return strings.Repeat("-", width)
	}
	filled := remaining * width / total
	if filled < 0 {
		filled = 0
	}
	if filled > width {
		filled = width
	}
	return "[" + strings.Repeat("#", filled) + strings.Repeat("-", width-filled) + "]"
}
