package main

import (
	"encoding/json"
	"testing"
)

func parse(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal %s: %v", b, err)
	}
	return m
}

func TestHTTPMCPEntry(t *testing.T) {
	withKey := httpMCPEntry("https://mcp.rindler.ai/mcp", "rindler_live_k")
	if withKey["type"] != "http" || withKey["url"] != "https://mcp.rindler.ai/mcp" {
		t.Fatalf("bad entry: %+v", withKey)
	}
	hdrs, ok := withKey["headers"].(map[string]any)
	if !ok || hdrs["Authorization"] != "Bearer rindler_live_k" {
		t.Fatalf("bad headers: %+v", withKey["headers"])
	}
	noKey := httpMCPEntry("https://mcp.rindler.ai/mcp", "")
	if _, has := noKey["headers"]; has {
		t.Error("no key => no headers")
	}
}

func TestUpsertPreservesAndIsIdempotent(t *testing.T) {
	existing := []byte(`{
  "numStartups": 7,
  "mcpServers": {
    "other": { "type": "stdio", "command": "foo" }
  }
}`)
	entry := httpMCPEntry("https://mcp.rindler.ai/mcp", "rindler_live_k")
	out, err := upsertMCPServerJSON(existing, "rindler", entry)
	if err != nil {
		t.Fatal(err)
	}
	doc := parse(t, out)
	// Preserved unrelated top-level key.
	if doc["numStartups"].(float64) != 7 {
		t.Error("dropped numStartups")
	}
	servers := doc["mcpServers"].(map[string]any)
	// Preserved the other server.
	if _, ok := servers["other"]; !ok {
		t.Error("dropped other server")
	}
	// Added rindler with the right shape.
	r := servers["rindler"].(map[string]any)
	if r["type"] != "http" || r["url"] != "https://mcp.rindler.ai/mcp" {
		t.Errorf("bad rindler entry: %+v", r)
	}
	// Idempotent: a second upsert yields identical bytes.
	out2, _ := upsertMCPServerJSON(out, "rindler", entry)
	if string(out) != string(out2) {
		t.Error("upsert not idempotent")
	}
	// Trailing newline (installer byte style).
	if out[len(out)-1] != '\n' {
		t.Error("missing trailing newline")
	}
}

func TestUpsertFromEmpty(t *testing.T) {
	out, err := upsertMCPServerJSON(nil, "rindler", httpMCPEntry("u", "k"))
	if err != nil {
		t.Fatal(err)
	}
	servers := parse(t, out)["mcpServers"].(map[string]any)
	if _, ok := servers["rindler"]; !ok {
		t.Error("rindler not added from empty")
	}
}

func TestUpsertRejectsInvalidJSON(t *testing.T) {
	if _, err := upsertMCPServerJSON([]byte("{not json"), "rindler", httpMCPEntry("u", "k")); err == nil {
		t.Error("expected error on invalid existing JSON (must not clobber)")
	}
}

func TestRemoveAndPresent(t *testing.T) {
	base, _ := upsertMCPServerJSON([]byte(`{"mcpServers":{"other":{"type":"stdio"}}}`), "rindler", httpMCPEntry("u", "k"))
	if !mcpServerPresentJSON(base, "rindler") {
		t.Fatal("should be present")
	}
	out, removed, err := removeMCPServerJSON(base, "rindler")
	if err != nil || !removed {
		t.Fatalf("remove: removed=%v err=%v", removed, err)
	}
	if mcpServerPresentJSON(out, "rindler") {
		t.Error("still present after remove")
	}
	// The other server survives.
	if !mcpServerPresentJSON(out, "other") {
		t.Error("removed the wrong server")
	}
	// Removing a missing entry => removed=false, no error.
	_, removed2, err := removeMCPServerJSON(out, "rindler")
	if err != nil || removed2 {
		t.Errorf("missing remove: removed=%v err=%v", removed2, err)
	}
}
