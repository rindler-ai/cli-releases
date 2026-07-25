package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Claude Code MCP install. The CLI writes a user-scope HTTP MCP server
// entry into ~/.claude.json (or $CLAUDE_CONFIG_DIR/.claude.json) under the
// top-level "mcpServers" object, matching the shape the dashboard
// emits: { "type": "http", "url": <mcpURL>, "headers": { "Authorization":
// "Bearer <key>" } }. The upsert is idempotent and preserves every other key and
// every other server.

// claudeConfigPath resolves Claude Code's user config file.
func claudeConfigPath() (string, error) {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return filepath.Join(d, ".claude.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude.json"), nil
}

// httpMCPEntry is the server object shared by Claude Code / Cursor / Windsurf.
func httpMCPEntry(mcpURL, key string) map[string]any {
	entry := map[string]any{"type": "http", "url": mcpURL}
	if key != "" {
		entry["headers"] = map[string]any{"Authorization": "Bearer " + key}
	}
	return entry
}

// upsertMCPServerJSON inserts/replaces mcpServers[name] in a Claude-style JSON
// document, preserving all other content. A nil/empty input starts from {}.
func upsertMCPServerJSON(existing []byte, name string, entry map[string]any) ([]byte, error) {
	doc := map[string]any{}
	if len(existing) > 0 {
		if err := json.Unmarshal(existing, &doc); err != nil {
			return nil, fmt.Errorf("existing config is not valid JSON: %w", err)
		}
	}
	servers, _ := doc["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	servers[name] = entry
	doc["mcpServers"] = servers
	return marshalConfigJSON(doc)
}

// removeMCPServerJSON deletes mcpServers[name] if present. removed reports
// whether an entry was actually removed.
func removeMCPServerJSON(existing []byte, name string) (out []byte, removed bool, err error) {
	if len(existing) == 0 {
		return existing, false, nil
	}
	doc := map[string]any{}
	if err := json.Unmarshal(existing, &doc); err != nil {
		return nil, false, fmt.Errorf("existing config is not valid JSON: %w", err)
	}
	servers, _ := doc["mcpServers"].(map[string]any)
	if servers == nil {
		return existing, false, nil
	}
	if _, ok := servers[name]; !ok {
		return existing, false, nil
	}
	delete(servers, name)
	doc["mcpServers"] = servers
	b, err := marshalConfigJSON(doc)
	return b, true, err
}

// mcpServerPresentJSON reports whether mcpServers[name] exists.
func mcpServerPresentJSON(existing []byte, name string) bool {
	if len(existing) == 0 {
		return false
	}
	doc := map[string]any{}
	if json.Unmarshal(existing, &doc) != nil {
		return false
	}
	servers, _ := doc["mcpServers"].(map[string]any)
	_, ok := servers[name]
	return ok
}

// marshalConfigJSON renders 2-space-indented JSON with a trailing newline,
// matching the installer's byte style.
func marshalConfigJSON(doc map[string]any) ([]byte, error) {
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// installClaudeCode upserts the rindler server into Claude Code's config.
func installClaudeCode(mcpURL, key string) (string, error) {
	p, err := claudeConfigPath()
	if err != nil {
		return "", err
	}
	existing, err := readFileOrEmpty(p)
	if err != nil {
		return "", err
	}
	out, err := upsertMCPServerJSON(existing, mcpServerName, httpMCPEntry(mcpURL, key))
	if err != nil {
		return "", err
	}
	if err := writeFilePreservePerm(p, out, 0o600); err != nil {
		return "", err
	}
	return p, nil
}

// removeClaudeCode deletes the rindler server from Claude Code's config.
func removeClaudeCode() (string, bool, error) {
	p, err := claudeConfigPath()
	if err != nil {
		return "", false, err
	}
	existing, err := readFileOrEmpty(p)
	if err != nil {
		return "", false, err
	}
	out, removed, err := removeMCPServerJSON(existing, mcpServerName)
	if err != nil || !removed {
		return p, removed, err
	}
	return p, true, writeFilePreservePerm(p, out, 0o600)
}

// statusClaudeCode reports whether rindler is configured.
func statusClaudeCode() (string, bool) {
	p, err := claudeConfigPath()
	if err != nil {
		return "", false
	}
	existing, _ := readFileOrEmpty(p)
	return p, mcpServerPresentJSON(existing, mcpServerName)
}
