package main

import (
	"fmt"
	"io"
)

// agentResult is the outcome of an install/remove/status op for one agent.
type agentResult struct {
	agent string
	path  string
	ok    bool
	note  string
	err   error
}

// installAllAgents writes the rindler MCP server into every supported agent
// config (Claude Code + Codex). Best-effort: one agent's failure never aborts the
// other.
func installAllAgents(mcpURL, key string) []agentResult {
	var out []agentResult

	p, err := installClaudeCode(mcpURL, key)
	out = append(out, agentResult{agent: "Claude Code", path: p, ok: err == nil, err: err})

	// No note on success. This used to tell every user to add
	// `experimental_use_rmcp_client = true`, a key Codex no longer has (and whose
	// class has hard-failed Codex startup), so a clean install permanently read as
	// half-broken while handing out advice that could break the thing it "fixed".
	p2, err2 := installCodex(mcpURL, key)
	out = append(out, agentResult{agent: "Codex", path: p2, ok: err2 == nil, err: err2})

	return out
}

// removeAllAgents removes the rindler server from every agent config.
func removeAllAgents() []agentResult {
	var out []agentResult

	p, removed, err := removeClaudeCode()
	out = append(out, agentResult{agent: "Claude Code", path: p, ok: err == nil, note: removedNote(removed), err: err})

	p2, removed2, err2 := removeCodex()
	out = append(out, agentResult{agent: "Codex", path: p2, ok: err2 == nil, note: removedNote(removed2), err: err2})

	return out
}

// statusAllAgents reports whether rindler is configured in each agent.
func statusAllAgents() []agentResult {
	var out []agentResult

	// Report the endpoint, not just the presence. "Installed" was true of an
	// entry pointing at a lane the user no longer uses -- which looks healthy
	// and sends every agent call somewhere else.
	want := mcpEndpoint(loadConfigOrEmpty())

	// The KEY matters as much as the URL: a config carrying a revoked or foreign
	// key looks installed while every tool call 401s, which is the exact
	// "installed but never connects" case users cannot diagnose.
	activeKey := ""
	if store, _, err := newCredentialStore(); err == nil && store != nil {
		activeKey, _, _ = resolveActiveKey(store)
	}

	p, present := statusClaudeCode()
	note := endpointNote(present, installedClaudeURL(), want)
	if present && !installedKeyMatches(installedClaudeKey(), activeKey) {
		note = keyMismatchNote("Claude Code")
	}
	out = append(out, agentResult{agent: "Claude Code", path: p, ok: present, note: note})

	p2, present2 := statusCodex()
	note2 := endpointNote(present2, installedCodexURL(), want)
	if present2 && !installedKeyMatches(installedCodexKey(), activeKey) {
		note2 = keyMismatchNote("Codex")
	}
	out = append(out, agentResult{agent: "Codex", path: p2, ok: present2, note: note2})

	return out
}

// endpointNote says which server is installed and flags a mismatch. An unknown
// installed URL (an older entry, or a shape we cannot read) falls back to the
// plain presence note rather than inventing a disagreement.
func endpointNote(present bool, installed, want string) string {
	if !present {
		return presentNote(false)
	}
	switch {
	case installed == "":
		return presentNote(true)
	case want != "" && installed != want:
		return "installed, but pointing at " + installed + " (expected " + want + "); run `rindler mcp install` to correct it"
	default:
		return "installed → " + installed
	}
}

// loadConfigOrEmpty is loadConfig without the error: for a status line, a
// missing config is a legitimate state, not a failure.
func loadConfigOrEmpty() cliConfig {
	cfg, _ := loadConfig()
	return cfg
}

func removedNote(removed bool) string {
	if removed {
		return "removed"
	}
	return "not present (nothing to remove)"
}

func presentNote(present bool) string {
	if present {
		return "configured"
	}
	return "not configured"
}

// printAgentResults writes a human summary of the results.
func printAgentResults(w io.Writer, verb string, results []agentResult) {
	for _, r := range results {
		switch {
		case r.err != nil:
			fmt.Fprintf(w, "  ✗ %s: %v\n", r.agent, r.err)
		case r.note != "":
			fmt.Fprintf(w, "  • %s — %s (%s)\n", r.agent, r.note, r.path)
		default:
			fmt.Fprintf(w, "  ✓ %s %s (%s)\n", r.agent, verb, r.path)
		}
	}
}
