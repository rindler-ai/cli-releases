package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// THIS REPO IS PUBLIC. It must not name files, packages, or issue numbers that
// live in the private monorepo.
//
// A security audit found seven such references and scrubbed them from the
// working tree and all of history. Writing the FIX, I put three more back --
// in comments explaining where the contract came from, which is the most
// natural place to want a path and exactly why a review pass will not catch it
// reliably. That is twice, so it gets a test rather than more care.
//
// What leaks here is not dramatic on its own: a filename tells an outside
// reader how our server is laid out and hands them the internal target name
// for a surface they can already reach. Small, cumulative, and free to avoid --
// every one of those comments says what it needs to without the path.
func TestNoPrivateRepoReferences(t *testing.T) {
	// Assembled from fragments so the forbidden literals never appear in this
	// file. The obvious alternative -- exempting the scanner from its own scan
	// -- is a hole: it would make this the one place a real leak could hide.
	pkg := "pack" + "ages"
	sib := strings.Join([]string{"user" + "-app", "tunnel" + "-daemon", "site" + "-engine", "mcp" + "-server"}, "|")
	tracker := "rindler" + "-ai/M" + "VP"

	patterns := []struct {
		name string
		re   *regexp.Regexp
	}{
		{"private monorepo package path", regexp.MustCompile(pkg + `/[a-z-]+/`)},
		{"private server source file", regexp.MustCompile(`\bapi/[a-z_]+\.go`)},
		{"private sibling package", regexp.MustCompile(`\b(` + sib + `)\b`)},
		{"private tracker reference", regexp.MustCompile(tracker)},
		{"private issue number", regexp.MustCompile(`(^|[^0-9a-fA-F#\w])#\d{4,5}\b`)},
	}
	// Public HTTP paths are not source paths. These are endpoints anyone can
	// call and are documented as such.
	allowed := regexp.MustCompile(`/api/(cli/token|cli/logout|credits/balance|workspace/usage/me)`)

	for _, path := range repoTextFiles(t) {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for i, line := range strings.Split(string(raw), "\n") {
			if allowed.MatchString(line) {
				continue
			}
			for _, p := range patterns {
				if m := p.re.FindString(line); m != "" {
					t.Errorf("%s:%d leaks a %s (%q) into a PUBLIC repo:\n  %s",
						path, i+1, p.name, m, strings.TrimSpace(line))
				}
			}
		}
	}
}

// repoTextFiles is every file a reader of this repo can see. Deliberately
// includes tests and workflows: a leak in a comment is as public as one in
// shipped code.
func repoTextFiles(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, dir := range []string{".", ".github/workflows"} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			if !strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, ".md") &&
				!strings.HasSuffix(name, ".yml") && !strings.HasSuffix(name, ".yaml") {
				continue
			}
			if dir == "." {
				out = append(out, name)
			} else {
				out = append(out, dir+"/"+name)
			}
		}
	}
	// A scanner that silently found nothing would be worse than none at all.
	if len(out) < 20 {
		t.Fatalf("only found %d files to scan; the walker is broken, not the repo", len(out))
	}
	return out
}
