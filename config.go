package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// On-disk config for the rindler CLI. Only NON-SECRET metadata lives here
// (~/.config/rindler/config.json); the rindler_live_ key itself lives in the OS
// keyring (see keyring.go), never in this file. The directory is
// ~/.config/rindler/, overridable via $RINDLER_CONFIG_DIR / XDG.

const (
	configDirName  = "rindler"
	configFileName = "config.json"
	// mcpServerName is the MCP server entry name written into Claude Code / Codex.
	mcpServerName = "rindler"
)

// cliConfig is the persisted non-secret session metadata.
type cliConfig struct {
	// Email is the signed-in Clerk account (display only).
	Email string `json:"email,omitempty"`
	// APIBase is the Rindler API origin the token was minted against
	// (e.g. https://mcp.rindler.ai). Also where /api/cli/token lives.
	APIBase string `json:"api_base,omitempty"`
	// AuthorizeBase is the dashboard origin serving the consent page
	// (e.g. https://app.rindler.ai).
	AuthorizeBase string `json:"authorize_base,omitempty"`
	// MCPURL is the MCP endpoint the key authenticates against
	// (e.g. https://mcp.rindler.ai/mcp), returned by the token endpoint.
	MCPURL string `json:"mcp_url,omitempty"`
	// Last4 is the last 4 chars of the active key (display + keyring account tag).
	Last4 string `json:"last4,omitempty"`
	// ExpiresAt is the key's expiry (RFC3339); drives the "login expires in N days"
	// warning.
	ExpiresAt string `json:"expires_at,omitempty"`
	// MapperAccess records whether the active key carries mapping capability.
	MapperAccess bool `json:"mapper_access,omitempty"`
	// ClerkUserID is the WORKSPACE the key is scoped to (the owner's clerk id),
	// which is a different account from the signer for a workspace member.
	ClerkUserID string `json:"clerk_user_id,omitempty"`
	// AccountClerkUserID is the signed-in account itself — the one Email names.
	// Empty against a server that predates the field.
	AccountClerkUserID string `json:"account_clerk_user_id,omitempty"`
}

// configDir resolves the config directory, honoring $RINDLER_CONFIG_DIR, then
// $XDG_CONFIG_HOME/rindler, else ~/.config/rindler.
func configDir() (string, error) {
	if d := os.Getenv("RINDLER_CONFIG_DIR"); d != "" {
		return d, nil
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, configDirName), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", configDirName), nil
}

func configPath() (string, error) {
	d, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, configFileName), nil
}

// loadConfig reads config.json. A missing file returns a zero cliConfig + nil err
// (logged-out is not an error).
func loadConfig() (cliConfig, error) {
	p, err := configPath()
	if err != nil {
		return cliConfig{}, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return cliConfig{}, nil
		}
		return cliConfig{}, err
	}
	var cfg cliConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cliConfig{}, err
	}
	return cfg, nil
}

// saveConfig writes config.json with dir 0700 / file 0600 (non-secret, but kept
// private for consistency with the credential fallback).
func saveConfig(cfg cliConfig) error {
	d, err := configDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(d, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(d, configFileName), b, 0o600)
}

// clearConfig removes config.json (used by logout). A missing file is not an error.
func clearConfig() error {
	p, err := configPath()
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// expiryStatus returns a human warning string and whether the key is expired,
// given the stored ExpiresAt and the current time. Empty ExpiresAt => ("", false).
func (c cliConfig) expiryStatus(now time.Time) (msg string, expired bool) {
	if c.ExpiresAt == "" {
		return "", false
	}
	exp, err := time.Parse(time.RFC3339, c.ExpiresAt)
	if err != nil {
		return "", false
	}
	if !now.Before(exp) {
		// A LOCAL snapshot taken at mint time. The server rolls expires_at forward
		// while the Clerk session stays alive, so this often reads "expired" for a
		// key that still works -- and following the advice mints a second key and
		// leaves the first one live. Say where the claim comes from.
		return "this machine's copy says the login expired (the server may have extended it) — `rindler doctor` checks with the server; `rindler login` renews", true
	}
	if d := exp.Sub(now); d < 3*24*time.Hour {
		return "login expires soon — run `rindler login` to renew", false
	}
	return "", false
}
