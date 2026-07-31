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
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// version is stamped at release time by the workflow
// (-ldflags "-X main.version=<tag without the rindler-cli-v prefix>"). It stays a
// var, not a const, precisely so that works: a const cannot be set by -X, which is
// how a hardcoded "0.1.0" would keep being reported by every later tag.
var version = "dev"

// installURL is the one-liner that installs or reinstalls this binary. It is a
// named constant so that a message telling someone to reinstall cannot drift
// from the command that actually does it -- an earlier draft pointed at
// an "upgrade" verb this CLI has never had, which is worse than saying
// nothing: it reads as an instruction and dead-ends.
const installURL = "https://rindler.ai/cli"

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
	case "run":
		return dispatchRun(args[1:])
	case "sites":
		return runSites(args[1:])
	case "actions":
		return runActions(args[1:])
	case "creds", "credentials":
		return runCreds(args[1:])
	case "usage":
		return runUsage(args[1:])
	case "sessions":
		return runSessions(args[1:])
	case "kill":
		return runKill(args[1:])
	case "vault":
		return runVault(args[1:])
	case "device", "devices":
		return runDevice(args[1:])
	case "doctor":
		return runDoctor(args[1:])
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
	fmt.Fprint(w, `rindler — run a task on a site

Usage:
  rindler run <site> "<what you want done>"      Say it in your own words; Rindler does it
  rindler login [--paste]                        Sign in
  rindler logout                                 Sign out on this machine
  rindler sites                                  The sites you can use
  rindler sites add <domain>                     Add a site to your workspace
  rindler creds add|list|show|rm                 Logins for a site, encrypted on this device
  rindler usage [--workspace] [--days N] [--json] Your automations, the same numbers the dashboard shows
  rindler sessions [--json]                      Browsers open on this machine
  rindler kill <name>                            Close one
  rindler vault status|enable|disable            Turn credential custody on this machine on or off
  rindler device status|list|serve               This machine as a paired device, and the relay
  rindler status                                 Whether you are signed in
  rindler whoami                                 The signed-in account
  rindler doctor                                 Diagnose a broken setup and print the fix
  rindler version                                Print the version

Examples:
  rindler run chase.com "download last month's statements"
  rindler run instacart.com "what do eggs cost at Costco"

The credential vault is OFF until you run "rindler vault enable": until then this
machine is not paired, is not listed on your dashboard, and no session can ask it
for a login.

Environment:
  RINDLER_API_KEY         Use this key instead of signing in (CI / headless; never persisted)
  RINDLER_CONFIG_DIR      Override the config dir (default ~/.config/rindler)
  RINDLER_AUTHORIZE_BASE  Override the dashboard origin (default https://app.rindler.ai)
  RINDLER_API_BASE        Override the API origin (default https://mcp.rindler.ai)
`)
}

// dispatchRun picks between the shapes of `run`.
//
// `rindler run <site> "<task>"` is the verb this CLI is FOR, so it is chosen on
// the shape of the arguments rather than behind an opt-in flag.
//
// The structured form (`--site X --action Y`) still works and is deliberately
// absent from the help. It is config vocabulary -- an action id is a name only
// someone who has read the mapping knows -- and the product is no longer sold
// to that person. Keeping it working strands nobody who already scripted
// against it; documenting it would put the layer we are hiding back on the
// first screen a new user reads.
func dispatchRun(args []string) int {
	switch runShapeFor(args) {
	case "status":
		return runRunStatus(args[1:])
	case "task":
		return runTask(args[0], args[1], args[2:])
	default:
		return runRun(args)
	}
}

// runShapeFor is the dispatch DECISION, split out so it can be tested without
// a network call or a config read. Picking wrong is silent in both directions:
// the structured form read as a sentence would build an automation out of two
// flag names, and `run status` read as the task verb would look for a site
// called "status".
func runShapeFor(args []string) string {
	if len(args) >= 1 && args[0] == "status" {
		return "status"
	}
	if len(args) >= 2 && !strings.HasPrefix(args[0], "-") && !strings.HasPrefix(args[1], "-") {
		return "task"
	}
	return "structured"
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
	//
	// Sweep EVERY backend, not just the preferred one. Which store is preferred
	// depends on an external binary being on PATH, so a key minted by an earlier
	// run can be sitting in the other backend; revoking only the preferred store
	// would leave a live key on disk while still printing "Logged out".
	stores, serr := allCredentialStores()
	if serr != nil || len(stores) == 0 {
		stores = []credentialStore{store}
	}
	// resolveAPIBase so RINDLER_API_BASE is honored here too: logging out of a
	// dev/self-hosted lane must revoke against THAT lane, not production.
	apiBase := resolveAPIBase("", cfg)
	revokedAny, seen := false, map[string]bool{}
	for _, st := range stores {
		key, _ := st.getKey()
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		outcome, _ := revokeSelf(ctx, defaultHTTPClient(), apiBase, key)
		cancel()
		switch outcome {
		case revokeDone:
			fmt.Println("✓ Key revoked server-side.")
			revokedAny = true
		case revokeNothingToDo:
			// Also a success, and the common one after a few days away: the key
			// lapsed with its Clerk session, so there was nothing live to retire.
			// Saying "revoked" would claim an action nobody took, and warning
			// would report a problem that does not exist.
			fmt.Println("✓ Key was already expired server-side; nothing to revoke.")
			revokedAny = true
		default:
			fmt.Println("• Could not revoke remotely (it expires with your Clerk session, or revoke it in the dashboard).")
		}
	}
	_ = revokedAny
	// Clear the key from every backend, so logout never leaves one behind.
	for _, st := range stores {
		if err := st.delKey(); err != nil {
			fmt.Fprintln(os.Stderr, "warning: could not clear stored key:", err)
		}
	}
	if err := clearConfig(); err != nil {
		fmt.Fprintln(os.Stderr, "warning: could not clear config:", err)
	}
	// Retire this machine's device enrollment too. Signing out of a machine that
	// still appears paired -- and still holds a device token and private key --
	// is the wrong default: the dashboard would keep offering to route logins to
	// a device that is no longer signed in.
	if deviceIsPaired() {
		dctx, dcancel := context.WithTimeout(context.Background(), 15*time.Second)
		err := unpairDevice(dctx, defaultHTTPClient())
		dcancel()
		var revokeErr *serverRevokeError
		switch {
		case errors.As(err, &revokeErr):
			// The key is gone either way, so custody really is off; be precise
			// about the half that did not happen rather than claiming both did.
			fmt.Println("✓ Erased this machine's device key.")
			fmt.Fprintf(os.Stderr, "warning: %v — remove it from the Devices list on your dashboard\n", err)
		case err != nil:
			fmt.Fprintln(os.Stderr, "warning: could not fully unpair this device:", err)
		default:
			fmt.Println("✓ Unpaired this machine.")
		}
	}
	fmt.Println("Cleaning up agent configuration written by older versions:")
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
	for _, line := range whoamiLines(cfg) {
		fmt.Println(line)
	}
	return 0
}

// whoamiLines renders `rindler whoami`. The whole job of whoami is "which
// account is this?", and an opaque Clerk id cannot answer that for anyone with
// a personal and a work account, or for an operator checking a machine — so we
// name the account when we know it.
//
// The subtlety is that a key carries TWO identities under MODEL B: the ACTOR
// who signed in (named by Email/AccountClerkUserID) and the SCOPE the key acts
// within (ClerkUserID, the workspace owner). For a member of someone else's
// workspace those are different accounts, so pairing the actor's email with the
// scope id printed "member@corp.com (user_<owner>)" — an email and an id
// belonging to two different people, which is worse than printing neither. Pair
// the email only with the actor id, and give the workspace its own labelled line
// when it differs.
func whoamiLines(cfg cliConfig) []string {
	var lines []string
	switch {
	case cfg.Email != "" && cfg.AccountClerkUserID != "":
		lines = append(lines, fmt.Sprintf("%s (%s)", cfg.Email, cfg.AccountClerkUserID))
	case cfg.Email != "":
		lines = append(lines, cfg.Email)
	case cfg.AccountClerkUserID != "":
		lines = append(lines, cfg.AccountClerkUserID)
	case cfg.ClerkUserID != "":
		// Scope only: the sole identity we hold, so print it unqualified rather
		// than label a workspace we cannot contrast with anything.
		lines = append(lines, cfg.ClerkUserID)
	default:
		lines = append(lines, fmt.Sprintf("logged in (key …%s)", cfg.Last4))
	}
	if cfg.ClerkUserID != "" && cfg.ClerkUserID != cfg.AccountClerkUserID && len(lines) > 0 &&
		lines[0] != cfg.ClerkUserID {
		lines = append(lines, "workspace: "+cfg.ClerkUserID)
	}
	return lines
}
