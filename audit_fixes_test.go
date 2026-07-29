package main

import (
	"errors"
	"strings"
	"testing"
)

// --- Codex duplicate-key refusal -------------------------------------------

// A rindler server spelled any way OTHER than our canonical header must be
// refused, not appended: appending produces a duplicate key, which makes
// config.toml unparseable and stops Codex starting at all.
func TestCodexRefusesForeignRindlerSpellings(t *testing.T) {
	cases := map[string]string{
		"quoted header":     "[mcp_servers.\"rindler\"]\nurl = \"https://old\"\n",
		"inline under mcp":  "[mcp_servers]\nrindler = { url = \"https://old\" }\n",
		"dotted key":        "mcp_servers.rindler.url = \"https://old\"\n",
		"spaced quoted hdr": "[ mcp_servers . \"rindler\" ]\nurl = \"https://old\"\n",
	}
	for name, existing := range cases {
		t.Run(name, func(t *testing.T) {
			out, err := upsertCodexTOML([]byte(existing), "https://mcp.rindler.ai/mcp", "k")
			if !errors.Is(err, errCodexForeignRindler) {
				t.Fatalf("expected refusal, got err=%v out=%q", err, string(out))
			}
			if out != nil {
				t.Error("must not return a config when refusing")
			}
		})
	}
}

// The canonical header is still REPLACED (not refused, not duplicated).
func TestCodexReplacesCanonicalHeader(t *testing.T) {
	existing := "[mcp_servers.rindler]\nurl = \"https://old\"\n\n[mcp_servers.keep]\ncommand = \"k\"\n"
	out, err := upsertCodexTOML([]byte(existing), "https://mcp.rindler.ai/mcp", "NEW")
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if strings.Count(s, "[mcp_servers.rindler]") != 1 {
		t.Fatalf("expected exactly one rindler header:\n%s", s)
	}
	if strings.Contains(s, "https://old") || !strings.Contains(s, "Bearer NEW") {
		t.Fatalf("not replaced:\n%s", s)
	}
	if !strings.Contains(s, "[mcp_servers.keep]") {
		t.Fatalf("clobbered a sibling server:\n%s", s)
	}
}

// An unrelated config is still appended to (no false refusal).
func TestCodexAppendsWhenNoRindler(t *testing.T) {
	out, err := upsertCodexTOML([]byte("model = \"gpt-5\"\n"), "https://mcp.rindler.ai/mcp", "k")
	if err != nil {
		t.Fatalf("unexpected refusal: %v", err)
	}
	if !strings.Contains(string(out), "[mcp_servers.rindler]") {
		t.Fatalf("not appended:\n%s", string(out))
	}
}

// A `rindler` key under a DIFFERENT table is not ours and must not trip the guard.
func TestCodexIgnoresRindlerKeyElsewhere(t *testing.T) {
	existing := "[profiles]\nrindler = \"something\"\n"
	if _, err := upsertCodexTOML([]byte(existing), "u", "k"); err != nil {
		t.Fatalf("false refusal on an unrelated table: %v", err)
	}
}

// --- API base resolution ----------------------------------------------------

// resolveAPIBase must honor RINDLER_API_BASE. `map status` and `logout` used to
// hand-roll a chain that skipped it, sending the Bearer key to PRODUCTION from a
// dev/self-hosted lane.
func TestResolveAPIBaseHonorsEnv(t *testing.T) {
	t.Setenv("RINDLER_API_BASE", "https://dev.example/")
	if got := resolveAPIBase("", cliConfig{APIBase: "https://cfg.example"}); got != "https://dev.example" {
		t.Fatalf("env must win over config, got %q", got)
	}
	// An explicit flag still outranks the env.
	if got := resolveAPIBase("https://flag.example", cliConfig{}); got != "https://flag.example" {
		t.Fatalf("flag must win, got %q", got)
	}
	t.Setenv("RINDLER_API_BASE", "")
	if got := resolveAPIBase("", cliConfig{}); got != defaultAPIBase {
		t.Fatalf("fallback should be the default, got %q", got)
	}
}
