package main

import (
	"flag"
	"io"
	"testing"
)

func newFS() (*flag.FlagSet, *string, *bool) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	base := fs.String("api-base", "", "")
	all := fs.Bool("all", false, "")
	return fs, base, all
}

// The bug this exists for: `rindler actions example.com --api-base https://dev`
// dropped --api-base, fell back to the DEFAULT origin, and talked to production
// while the user was naming a dev host. It reported prod's 401 for a dev call.
func TestFlagsAfterPositionalAreParsed(t *testing.T) {
	fs, base, all := newFS()
	rest, err := parseAnyOrder(fs, []string{"example.com", "--api-base", "https://dev.example", "--all"})
	if err != nil {
		t.Fatalf("parseAnyOrder errored: %v", err)
	}
	if *base != "https://dev.example" {
		t.Errorf("--api-base after the positional was dropped, got %q", *base)
	}
	if !*all {
		t.Error("--all after the positional was dropped")
	}
	if len(rest) != 1 || rest[0] != "example.com" {
		t.Errorf("positional = %v", rest)
	}
}

func TestFlagsBeforePositionalStillWork(t *testing.T) {
	fs, base, all := newFS()
	rest, err := parseAnyOrder(fs, []string{"--api-base", "https://x", "--all", "example.com"})
	if err != nil {
		t.Fatalf("errored: %v", err)
	}
	if *base != "https://x" || !*all || len(rest) != 1 || rest[0] != "example.com" {
		t.Errorf("base=%q all=%v rest=%v", *base, *all, rest)
	}
}

func TestEqualsFormDoesNotSwallowNextToken(t *testing.T) {
	fs, base, _ := newFS()
	rest, err := parseAnyOrder(fs, []string{"--api-base=https://x", "example.com"})
	if err != nil {
		t.Fatalf("errored: %v", err)
	}
	if *base != "https://x" || len(rest) != 1 || rest[0] != "example.com" {
		t.Errorf("base=%q rest=%v", *base, rest)
	}
}

// A bool flag takes no value, so the token after it is a POSITIONAL. Consuming
// it would silently eat the site name.
func TestBoolFlagDoesNotConsumeThePositional(t *testing.T) {
	fs, _, all := newFS()
	rest, err := parseAnyOrder(fs, []string{"--all", "example.com"})
	if err != nil {
		t.Fatalf("errored: %v", err)
	}
	if !*all || len(rest) != 1 || rest[0] != "example.com" {
		t.Errorf("all=%v rest=%v", *all, rest)
	}
}

func TestDoubleDashEndsFlagParsing(t *testing.T) {
	fs, base, _ := newFS()
	rest, err := parseAnyOrder(fs, []string{"--api-base", "https://x", "--", "--not-a-flag"})
	if err != nil {
		t.Fatalf("errored: %v", err)
	}
	if *base != "https://x" {
		t.Errorf("base = %q", *base)
	}
	if len(rest) != 1 || rest[0] != "--not-a-flag" {
		t.Errorf("after -- everything is positional, got %v", rest)
	}
}

func TestMultiplePositionalsPreserveOrder(t *testing.T) {
	fs, _, _ := newFS()
	rest, err := parseAnyOrder(fs, []string{"a", "--all", "b", "c"})
	if err != nil {
		t.Fatalf("errored: %v", err)
	}
	if len(rest) != 3 || rest[0] != "a" || rest[1] != "b" || rest[2] != "c" {
		t.Errorf("order lost: %v", rest)
	}
}

func TestUnknownFlagStillErrors(t *testing.T) {
	fs, _, _ := newFS()
	if _, err := parseAnyOrder(fs, []string{"example.com", "--nope"}); err == nil {
		t.Error("an unknown flag must still be an error, not silently ignored")
	}
}
