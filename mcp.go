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

	p2, err2 := installCodex(mcpURL, key)
	note := ""
	if err2 == nil {
		note = "if Codex doesn't pick it up, add `experimental_use_rmcp_client = true` to the top of the file or upgrade Codex"
	}
	out = append(out, agentResult{agent: "Codex", path: p2, ok: err2 == nil, note: note, err: err2})

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

	p, present := statusClaudeCode()
	out = append(out, agentResult{agent: "Claude Code", path: p, ok: present, note: presentNote(present)})

	p2, present2 := statusCodex()
	out = append(out, agentResult{agent: "Codex", path: p2, ok: present2, note: presentNote(present2)})

	return out
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
