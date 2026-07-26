// Command rindler is the Rindler CLI. It logs a user in via their Clerk
// account (OAuth 2.0 Authorization Code + PKCE), receives a temporary MCP key
// bound to their Clerk session, stores it in the OS keyring, and installs the
// Rindler MCP into Claude Code and Codex.
//
// Usage:
//
//	rindler login [--paste] [--no-map] [--no-mcp]
//	rindler run --site <domain> --action <name> [--input k=v]
//	rindler map <url> [--mode fast|deep]
//	rindler map status <job-id>
//	rindler logout
//	rindler status
//	rindler whoami
//	rindler mcp install|status|remove
//	rindler version
package main

import (
	"context"
	"fmt"
	"os"
	"time"
)

// version is stamped at release time by the workflow
// (-ldflags "-X main.version=<tag without the rindler-cli-v prefix>"). It stays a
// var, not a const, precisely so that works: a const cannot be set by -X, which is
// how a hardcoded "0.1.0" would keep being reported by every later tag.
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage(os.Stderr)
		return 2
	}
	switch args[0] {
	case "login":
		return runLogin(args[1:])
	case "logout":
		return runLogout(args[1:])
	case "status":
		return runStatus()
	case "whoami":
		return runWhoami()
	case "map":
		return runMap(args[1:])
	case "run":
		if len(args) >= 2 && args[1] == "status" {
			return runRunStatus(args[2:])
		}
		return runRun(args[1:])
	case "sites":
		return runSites(args[1:])
	case "actions":
		return runActions(args[1:])
	case "doctor":
		return runDoctor(args[1:])
	case "mcp":
		return runMCP(args[1:])
	// `rindler install mcp` reads as naturally as `rindler mcp install`, and
	// guessing wrong should not be a usage error.
	case "install":
		return runMCP(append([]string{"install"}, args[1:]...))
	case "version", "--version", "-v":
		fmt.Println("rindler", version)
		return 0
	case "help", "--help", "-h":
		usage(os.Stdout)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", args[0])
		usage(os.Stderr)
		return 2
	}
}

func usage(w *os.File) {
	fmt.Fprint(w, `rindler — the Rindler CLI

Usage:
  rindler login [--paste] [--no-map] [--no-mcp]  Sign in with Clerk, mint a session-bound MCP key,
                                                 and install the MCP into Claude Code + Codex
  rindler run --site <d> --action <a>            Run actions against a site and follow the job
  rindler run status <job-id> [--once]          Follow a run you already started
  rindler sites                                  List the sites you can act on
  rindler actions <site>                         Show a site's actions and their inputs
  rindler map <url> [--mode fast|deep]           Map a site and follow the run to a verdict
  rindler map status <job-id> [--once]           Follow a run you already started
  rindler logout                                 Revoke the key and remove local + agent config
  rindler status                                 Show login + MCP install status
  rindler whoami                                 Show the signed-in account
  rindler mcp install|status|remove              Manage the MCP install for Claude Code + Codex
  rindler doctor                                 Diagnose a broken setup and print the fix
  rindler version                                Print the version

Site mapping is requested at login by default; --no-map opts out. It is granted
only if your workspace is entitled, and "rindler status" reports which you got.

Environment:
  RINDLER_API_KEY         Use this key instead of logging in (CI / headless; never persisted)
  RINDLER_CONFIG_DIR      Override the config dir (default ~/.config/rindler)
  RINDLER_AUTHORIZE_BASE  Override the dashboard consent origin (default https://app.rindler.ai)
  RINDLER_API_BASE        Override the API origin (default https://mcp.rindler.ai)
`)
}

// runLogout revokes the key server-side (best-effort) and clears local + agent
// configuration.
func runLogout(args []string) int {
	cfg, _ := loadConfig()
	store, _, err := newCredentialStore()
	if err != nil {
		fmt.Fprintln(os.Stderr, "logout:", err)
		return 1
	}
	// Best-effort server-side revoke of the stored key (never the env key).
	if key, _ := store.getKey(); key != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		apiBase := cfg.APIBase
		if apiBase == "" {
			apiBase = defaultAPIBase
		}
		if ok, _ := revokeSelf(ctx, defaultHTTPClient(), apiBase, key); ok {
			fmt.Println("✓ Key revoked server-side.")
		} else {
			fmt.Println("• Could not revoke remotely (it expires with your Clerk session, or revoke it in the dashboard).")
		}
	}
	if err := store.delKey(); err != nil {
		fmt.Fprintln(os.Stderr, "warning: could not clear keyring:", err)
	}
	if err := clearConfig(); err != nil {
		fmt.Fprintln(os.Stderr, "warning: could not clear config:", err)
	}
	fmt.Println("Removing the Rindler MCP from your agents:")
	printAgentResults(os.Stdout, "removed", removeAllAgents())
	fmt.Println("\n✓ Logged out.")
	return 0
}

// runStatus prints login + MCP install status.
func runStatus() int {
	cfg, _ := loadConfig()
	store, warning, _ := newCredentialStore()
	key, src := "", ""
	if store != nil {
		key, src, _ = resolveActiveKey(store)
	}
	fmt.Println("Login:")
	if key == "" {
		fmt.Println("  not logged in — run `rindler login`")
	} else {
		if os.Getenv("RINDLER_API_KEY") != "" {
			fmt.Println("  using RINDLER_API_KEY from the environment")
		} else {
			fmt.Printf("  logged in — key …%s%s (stored in %s)\n", cfg.Last4, mapNote(cfg.MapperAccess), src)
			if cfg.ExpiresAt != "" {
				fmt.Printf("  expires %s\n", cfg.ExpiresAt)
			}
			if msg, _ := cfg.expiryStatus(time.Now()); msg != "" {
				fmt.Printf("  ⚠ %s\n", msg)
			}
		}
	}
	if warning != "" {
		fmt.Printf("  note: %s\n", warning)
	}
	fmt.Println("\nMCP install:")
	printAgentResults(os.Stdout, "configured", statusAllAgents())
	return 0
}

// runWhoami prints the signed-in account, or a non-zero exit if logged out.
func runWhoami() int {
	if env := os.Getenv("RINDLER_API_KEY"); env != "" {
		fmt.Println("authenticated via RINDLER_API_KEY (environment)")
		return 0
	}
	cfg, _ := loadConfig()
	store, _, _ := newCredentialStore()
	key := ""
	if store != nil {
		key, _ = store.getKey()
	}
	if key == "" {
		fmt.Fprintln(os.Stderr, "not logged in")
		return 1
	}
	if cfg.ClerkUserID != "" {
		fmt.Println(cfg.ClerkUserID)
	} else {
		fmt.Printf("logged in (key …%s)\n", cfg.Last4)
	}
	return 0
}

// runMCP handles `rindler mcp <install|status|remove>`.
func runMCP(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: rindler mcp install|status|remove")
		return 2
	}
	switch args[0] {
	case "install":
		cfg, _ := loadConfig()
		store, _, err := newCredentialStore()
		if err != nil {
			fmt.Fprintln(os.Stderr, "mcp install:", err)
			return 1
		}
		key, _, err := resolveActiveKey(store)
		if err != nil {
			fmt.Fprintln(os.Stderr, "mcp install:", err)
			return 1
		}
		if key == "" {
			fmt.Fprintln(os.Stderr, "not logged in — run `rindler login` first (or set RINDLER_API_KEY)")
			return 1
		}
		fmt.Println("Installing the Rindler MCP into your agents:")
		printAgentResults(os.Stdout, "configured", installAllAgents(mcpEndpoint(cfg), key))
		fmt.Println("\nRestart Claude Code / Codex to connect.")
		return 0
	case "status":
		printAgentResults(os.Stdout, "configured", statusAllAgents())
		return 0
	case "remove":
		fmt.Println("Removing the Rindler MCP from your agents:")
		printAgentResults(os.Stdout, "removed", removeAllAgents())
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown mcp subcommand %q (want install|status|remove)\n", args[0])
		return 2
	}
}
