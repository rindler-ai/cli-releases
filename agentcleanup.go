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

// removeAllAgents removes the rindler server from every agent config.
func removeAllAgents() []agentResult {
	var out []agentResult

	p, removed, err := removeClaudeCode()
	out = append(out, agentResult{agent: "Claude Code", path: p, ok: err == nil, note: removedNote(removed), err: err})

	p2, removed2, err2 := removeCodex()
	out = append(out, agentResult{agent: "Codex", path: p2, ok: err2 == nil, note: removedNote(removed2), err: err2})

	return out
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

func removedNote(removed bool) string {
	if removed {
		return "removed"
	}
	return "not present (nothing to remove)"
}
