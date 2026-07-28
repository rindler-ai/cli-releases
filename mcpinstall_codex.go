package main

import (
	"os"
	"path/filepath"
	"strings"
)

// Codex MCP install. Codex reads ~/.codex/config.toml (or
// $CODEX_HOME/config.toml). We upsert a first-class remote streamable-HTTP MCP
// server table:
//
//	[mcp_servers.rindler]
//	url = "https://mcp.rindler.ai/mcp"
//	http_headers = { "Authorization" = "Bearer <key>" }
//
// The Bearer is embedded inline (same posture as the Claude Code ~/.claude.json
// header and mcp-config.ts) so login is zero-friction — no env var to export.
// The upsert is SURGICAL: it replaces exactly the [mcp_servers.rindler] table and
// leaves every other line (comments, ordering, other servers, model settings)
// byte-for-byte intact, so we never need a lossy TOML round-trip.

const codexTableHeader = "[mcp_servers.rindler]"

// codexConfigPath resolves Codex's config file.
func codexConfigPath() (string, error) {
	if d := os.Getenv("CODEX_HOME"); d != "" {
		return filepath.Join(d, "config.toml"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex", "config.toml"), nil
}

// codexBlock renders the rindler table (no trailing newline).
func codexBlock(mcpURL, key string) string {
	// TOML basic-string escaping for the two interpolated values (URL + key are
	// controlled/simple, but escape defensively).
	return codexTableHeader + "\n" +
		"url = \"" + tomlEscape(mcpURL) + "\"\n" +
		"http_headers = { \"Authorization\" = \"Bearer " + tomlEscape(key) + "\" }"
}

func tomlEscape(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	return strings.ReplaceAll(s, "\"", "\\\"")
}

// isTOMLTableHeader reports whether a line is a TOML table header ([...] or
// [[...]]) — i.e. its first non-space rune is '['. Inline arrays/tables appear
// only inside `key = ...` value lines, which never start with '['.
func isTOMLTableHeader(line string) bool {
	return strings.HasPrefix(strings.TrimSpace(line), "[")
}

// upsertCodexTOML inserts or replaces the [mcp_servers.rindler] table, preserving
// all other content.
func upsertCodexTOML(existing []byte, mcpURL, key string) []byte {
	block := codexBlock(mcpURL, key)
	if len(existing) == 0 {
		return []byte(block + "\n")
	}
	lines := strings.Split(strings.TrimRight(string(existing), "\n"), "\n")
	start := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == codexTableHeader {
			start = i
			break
		}
	}
	if start == -1 {
		// Append a blank separator + our block.
		return []byte(strings.Join(lines, "\n") + "\n\n" + block + "\n")
	}
	// The table runs until the next table header (or EOF).
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if isTOMLTableHeader(lines[i]) {
			end = i
			break
		}
	}
	out := append([]string{}, lines[:start]...)
	out = append(out, strings.Split(block, "\n")...)
	out = append(out, lines[end:]...)
	return []byte(strings.Join(out, "\n") + "\n")
}

// removeCodexTOML deletes the [mcp_servers.rindler] table if present.
func removeCodexTOML(existing []byte) (out []byte, removed bool) {
	if len(existing) == 0 {
		return existing, false
	}
	lines := strings.Split(strings.TrimRight(string(existing), "\n"), "\n")
	start := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == codexTableHeader {
			start = i
			break
		}
	}
	if start == -1 {
		return existing, false
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if isTOMLTableHeader(lines[i]) {
			end = i
			break
		}
	}
	// Also drop a single trailing blank line left by the removed block, if any.
	kept := append([]string{}, lines[:start]...)
	tail := lines[end:]
	if len(kept) > 0 && kept[len(kept)-1] == "" && len(tail) > 0 && tail[0] == "" {
		tail = tail[1:]
	}
	kept = append(kept, tail...)
	joined := strings.TrimRight(strings.Join(kept, "\n"), "\n")
	if joined == "" {
		return []byte{}, true
	}
	return []byte(joined + "\n"), true
}

// codexTablePresent reports whether the rindler table exists.
func codexTablePresent(existing []byte) bool {
	for _, l := range strings.Split(string(existing), "\n") {
		if strings.TrimSpace(l) == codexTableHeader {
			return true
		}
	}
	return false
}

func installCodex(mcpURL, key string) (string, error) {
	p, err := codexConfigPath()
	if err != nil {
		return "", err
	}
	existing, err := readFileOrEmpty(p)
	if err != nil {
		return "", err
	}
	if err := writeFilePreservePerm(p, upsertCodexTOML(existing, mcpURL, key), 0o600); err != nil {
		return "", err
	}
	return p, nil
}

func removeCodex() (string, bool, error) {
	p, err := codexConfigPath()
	if err != nil {
		return "", false, err
	}
	existing, err := readFileOrEmpty(p)
	if err != nil {
		return "", false, err
	}
	out, removed := removeCodexTOML(existing)
	if !removed {
		return p, false, nil
	}
	return p, true, writeFilePreservePerm(p, out, 0o600)
}

func statusCodex() (string, bool) {
	p, err := codexConfigPath()
	if err != nil {
		return "", false
	}
	existing, _ := readFileOrEmpty(p)
	return p, codexTablePresent(existing)
}

// installedCodexURL reports the endpoint currently in Codex's config, so status
// can name WHICH server is installed rather than only that one is -- the same
// readback Claude Code already has.
//
// A deliberately small parser rather than a TOML dependency: this repo is
// stdlib-only apart from the websocket client, and all this needs is the `url`
// line inside our own table. It stops at the next table header, so a `url` key
// belonging to some other MCP server cannot be mistaken for ours.
func installedCodexURL() string {
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
		trimmed := strings.TrimSpace(line)
		if trimmed == codexTableHeader {
			inOurTable = true
			continue
		}
		// Any OTHER table header ends ours. Without this a later server's url
		// would be read as ours and status would report a mismatch that is not.
		if inOurTable && isTOMLTableHeader(trimmed) {
			return ""
		}
		if !inOurTable {
			continue
		}
		key, value, found := strings.Cut(trimmed, "=")
		if !found || strings.TrimSpace(key) != "url" {
			continue
		}
		return strings.Trim(strings.TrimSpace(value), `"`)
	}
	return ""
}
