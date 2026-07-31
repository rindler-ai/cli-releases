package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// Every command this CLI NAMES to a user must be a command it HAS.
//
// This exists because a refusal shipped naming a verb the CLI has never had
// (see installURL's comment in main.go for which one). A message like that is
// worse than no message: the reader follows the instruction, gets "unknown
// command", and now has two problems instead of one. The same trap already bit
// the relay once, whose disabled-hint pointed at the login verb long after
// pairing had moved to `vault enable`.
//
// The compiler cannot help here. These are strings, so one naming a verb that
// does not exist builds fine and ships fine; grepping the source is the only
// thing that catches it.
//
// Scope is deliberately backtick-quoted references only. That is this repo's
// convention for "this is a command", and it keeps the check off ordinary
// prose like "the rindler table" or "rindler is not installed", which would
// otherwise need an exception list long enough to hide a real miss.
func TestEveryCommandWeNameActuallyExists(t *testing.T) {
	top := topLevelVerbs(t)
	ref := regexp.MustCompile("`rindler ([a-z][a-z-]*)")

	for _, path := range goSourceFiles(t) {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, m := range ref.FindAllStringSubmatch(string(src), -1) {
			if !top[m[1]] {
				t.Errorf("%s names `rindler %s`, which is not a command this CLI has.\n"+
					"Either add the command or name one that exists — a dead-end instruction is worse than none.",
					path, m[1])
			}
		}
	}
}

// topLevelVerbs reads the dispatch switch in run() rather than a hand-kept
// list, so DELETING a command makes every message that still mentions it fail
// here. A hardcoded list would just go stale alongside the messages.
//
// It stops at runVault, because status/enable/disable are vault SUBcommands:
// counting them as top-level would make `rindler install` look valid when the
// real command is `rindler vault enable`.
func topLevelVerbs(t *testing.T) map[string]bool {
	t.Helper()
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	body := string(src)
	if cut := strings.Index(body, "func runVault"); cut > 0 {
		body = body[:cut]
	}

	verbs := map[string]bool{}
	for _, m := range regexp.MustCompile(`case ((?:"[a-z-]+"(?:, )?)+):`).FindAllStringSubmatch(body, -1) {
		for _, q := range strings.Split(m[1], ", ") {
			verbs[strings.Trim(q, `"`)] = true
		}
	}
	// A parser that silently matched nothing would make this test vacuously
	// green — the exact failure mode it exists to prevent elsewhere.
	if len(verbs) < 10 {
		t.Fatalf("parsed only %d verbs from main.go — the parser is broken, not the CLI", len(verbs))
	}
	for _, must := range []string{"login", "run", "usage", "vault", "device", "doctor"} {
		if !verbs[must] {
			t.Fatalf("parser missed the %q verb; it is not trustworthy", must)
		}
	}
	return verbs
}

func goSourceFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() && strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") {
			out = append(out, name)
		}
	}
	if len(out) == 0 {
		t.Fatal("found no source files to scan")
	}
	return out
}

// The install one-liner is quoted in refusals, so if it drifts from the real
// installer every one of those messages sends people somewhere wrong.
func TestInstallURLIsThePublishedOne(t *testing.T) {
	if installURL != "https://rindler.ai/cli" {
		t.Fatalf("installURL = %q; the published installer is https://rindler.ai/cli", installURL)
	}
}
