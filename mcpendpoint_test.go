package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A wrong MCP endpoint is worse than most wrong values, because it persists in
// a file the user will not think to check: every later agent call goes to the
// wrong lane and nothing in the CLI mentions it again.
func TestMcpEndpointHonoursTheOverride(t *testing.T) {
	t.Setenv("RINDLER_API_BASE", "https://staging.example")
	// The server-returned URL is still authoritative when present: it is the
	// endpoint the lane told us to use.
	if got := mcpEndpoint(cliConfig{MCPURL: "https://given.example/mcp"}); got != "https://given.example/mcp" {
		t.Errorf("the server-returned MCP URL must win, got %q", got)
	}
	// With none, the override must beat the production default. This is the CI
	// case: RINDLER_API_KEY set, never logged in, so no saved config.
	if got := mcpEndpoint(cliConfig{}); got != "https://staging.example/mcp" {
		t.Errorf("mcpEndpoint = %q; it installed the wrong lane into the agent config", got)
	}
}

func TestMcpEndpointFallsBackToTheDefault(t *testing.T) {
	t.Setenv("RINDLER_API_BASE", "")
	if got := mcpEndpoint(cliConfig{}); got != defaultAPIBase+"/mcp" {
		t.Errorf("mcpEndpoint = %q, want the default lane", got)
	}
}

// "Installed" was true of an entry pointing at a lane the user no longer uses:
// healthy-looking, and sending every agent call somewhere else.
func TestStatusFlagsAnEndpointMismatch(t *testing.T) {
	for _, tc := range []struct {
		name, installed, want string
		present               bool
		expect                string
	}{
		{"points elsewhere", "https://old.example/mcp", "https://new.example/mcp", true, "pointing at"},
		{"agrees", "https://new.example/mcp", "https://new.example/mcp", true, "installed →"},
		{"not installed", "", "https://new.example/mcp", false, "not"},
		// Unknown installed URL must NOT invent a disagreement.
		{"unreadable entry", "", "https://new.example/mcp", true, "configured"},
	} {
		got := endpointNote(tc.present, tc.installed, tc.want)
		if !strings.Contains(got, tc.expect) {
			t.Errorf("%s: note %q does not contain %q", tc.name, got, tc.expect)
		}
		if tc.name == "unreadable entry" && strings.Contains(got, "pointing at") {
			t.Errorf("%s: invented a mismatch from an unknown URL: %q", tc.name, got)
		}
	}
}

// The readback must parse what the installer actually writes.
func TestInstalledURLReadsWhatInstallWrote(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	doc := map[string]any{"mcpServers": map[string]any{
		mcpServerName: map[string]any{"type": "http", "url": "https://written.example/mcp"},
	}}
	b, _ := json.MarshalIndent(doc, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, ".claude.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := installedClaudeURL(); got != "https://written.example/mcp" {
		t.Fatalf("installedClaudeURL = %q, want what install wrote", got)
	}
}
