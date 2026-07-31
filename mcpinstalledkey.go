package main

import (
	"encoding/json"
	"strings"
)

// Reading the key that is actually INSTALLED in each agent's config.
//
// status/doctor used to compare only the URL, so a config still carrying a
// revoked or foreign key reported green while every tool call from Claude Code
// or Codex 401'd -- the exact "installed but never connects" case, with the
// CLI's own diagnostics pointing away from the cause.

// bearerFromHeader extracts "<key>" from "Bearer <key>" (case-insensitive).
func bearerFromHeader(v string) string {
	const p = "bearer "
	if len(v) >= len(p) && strings.EqualFold(v[:len(p)], p) {
		return strings.TrimSpace(v[len(p):])
	}
	return ""
}

// installedClaudeKey returns the key in Claude Code's rindler entry, or "".
func installedClaudeKey() string {
	p, err := claudeConfigPath()
	if err != nil {
		return ""
	}
	existing, err := readFileOrEmpty(p)
	if err != nil || len(existing) == 0 {
		return ""
	}
	var doc map[string]any
	if json.Unmarshal(existing, &doc) != nil {
		return ""
	}
	servers, _ := doc["mcpServers"].(map[string]any)
	entry, _ := servers[mcpServerName].(map[string]any)
	headers, _ := entry["headers"].(map[string]any)
	auth, _ := headers["Authorization"].(string)
	return bearerFromHeader(auth)
}

// installedCodexKey returns the key in Codex's rindler table, or "".
func installedCodexKey() string {
	p, err := codexConfigPath()
	if err != nil {
		return ""
	}
	existing, err := readFileOrEmpty(p)
	if err != nil || len(existing) == 0 {
		return ""
	}
	inOurTable := false
	for _, line := range strings.Split(string(existing), "\n") {
		t := strings.TrimSpace(line)
		if t == codexTableHeader {
			inOurTable = true
			continue
		}
		if inOurTable && isTOMLTableHeader(t) {
			break
		}
		if !inOurTable || !strings.HasPrefix(t, "http_headers") {
			continue
		}
		// http_headers = { "Authorization" = "Bearer <key>" }
		if i := strings.Index(t, "Bearer "); i >= 0 {
			rest := t[i+len("Bearer "):]
			if j := strings.IndexAny(rest, `"`); j >= 0 {
				return strings.TrimSpace(rest[:j])
			}
		}
	}
	return ""
}

// installedKeyMatches reports whether the key installed in an agent config is the
// one currently active. "" installed (nothing there) is not a mismatch.
func installedKeyMatches(installed, active string) bool {
	if installed == "" || active == "" {
		return true
	}
	return installed == active
}

// keyMismatchNote is the warning shown when an agent holds a different key than
// the active one -- the tool calls will 401 even though the server is installed.
func keyMismatchNote(agent string) string {
	return "installed key is not your current one, so " + agent +
		" will get 401s — run `rindler login` again"
}
