package main

import (
	"strings"
	"testing"
)

const mcpURL = "https://mcp.rindler.ai/mcp"

func TestCodexUpsertEmpty(t *testing.T) {
	out := mustUpsertCodex(t, nil, mcpURL, "rindler_live_k")
	want := "[mcp_servers.rindler]\n" +
		"url = \"https://mcp.rindler.ai/mcp\"\n" +
		"http_headers = { \"Authorization\" = \"Bearer rindler_live_k\" }\n"
	if out != want {
		t.Fatalf("empty upsert:\n got %q\nwant %q", out, want)
	}
}

func TestCodexUpsertAppendsPreservingRest(t *testing.T) {
	existing := []byte(`# my codex config
model = "gpt-5"

[mcp_servers.other]
command = "foo"
args = ["--bar"]
`)
	out := mustUpsertCodex(t, existing, mcpURL, "k1")
	// Everything preserved.
	for _, must := range []string{"# my codex config", `model = "gpt-5"`, "[mcp_servers.other]", `command = "foo"`} {
		if !strings.Contains(out, must) {
			t.Errorf("lost line %q in:\n%s", must, out)
		}
	}
	if !strings.Contains(out, "[mcp_servers.rindler]") || !strings.Contains(out, "Bearer k1") {
		t.Errorf("rindler table not appended:\n%s", out)
	}
	// Idempotent.
	out2 := mustUpsertCodex(t, []byte(out), mcpURL, "k1")
	if out != out2 {
		t.Errorf("not idempotent:\n%q\nvs\n%q", out, out2)
	}
}

func TestCodexUpsertReplacesInMiddle(t *testing.T) {
	existing := []byte(`[mcp_servers.rindler]
url = "https://old.example/mcp"
http_headers = { "Authorization" = "Bearer OLDKEY" }

[mcp_servers.keep]
command = "keep"
`)
	out := mustUpsertCodex(t, existing, mcpURL, "NEWKEY")
	if strings.Contains(out, "OLDKEY") || strings.Contains(out, "old.example") {
		t.Errorf("old table not replaced:\n%s", out)
	}
	if !strings.Contains(out, "Bearer NEWKEY") || !strings.Contains(out, mcpURL) {
		t.Errorf("new values missing:\n%s", out)
	}
	// The trailing table survived intact.
	if !strings.Contains(out, "[mcp_servers.keep]") || !strings.Contains(out, `command = "keep"`) {
		t.Errorf("clobbered the following table:\n%s", out)
	}
	// Exactly one rindler header.
	if n := strings.Count(out, "[mcp_servers.rindler]"); n != 1 {
		t.Errorf("expected 1 rindler header, got %d", n)
	}
}

func TestCodexRemove(t *testing.T) {
	existing := []byte(`model = "gpt-5"

[mcp_servers.rindler]
url = "u"
http_headers = { "Authorization" = "Bearer k" }

[mcp_servers.keep]
command = "keep"
`)
	out, removed := removeCodexTOML(existing)
	if !removed {
		t.Fatal("expected removed")
	}
	s := string(out)
	if strings.Contains(s, "[mcp_servers.rindler]") || strings.Contains(s, "Bearer k") {
		t.Errorf("rindler not removed:\n%s", s)
	}
	if !strings.Contains(s, `model = "gpt-5"`) || !strings.Contains(s, "[mcp_servers.keep]") {
		t.Errorf("removed too much:\n%s", s)
	}
	// Removing again is a no-op.
	if _, removed2 := removeCodexTOML(out); removed2 {
		t.Error("second remove should be false")
	}
}

func TestCodexPresent(t *testing.T) {
	if codexTablePresent([]byte("model=\"x\"\n")) {
		t.Error("false positive")
	}
	if !codexTablePresent([]byte("[mcp_servers.rindler]\nurl=\"u\"\n")) {
		t.Error("false negative")
	}
}

func TestTOMLEscape(t *testing.T) {
	if got := tomlEscape(`a"b\c`); got != `a\"b\\c` {
		t.Errorf("escape = %q", got)
	}
}

// mustUpsertCodex is the test helper for the (bytes, error) signature: the
// duplicate-key refusal is covered by its own test, so these cases assert on the
// happy path.
func mustUpsertCodex(t *testing.T, existing []byte, mcpURL, key string) string {
	t.Helper()
	b, err := upsertCodexTOML(existing, mcpURL, key)
	if err != nil {
		t.Fatalf("upsertCodexTOML: %v", err)
	}
	return string(b)
}
