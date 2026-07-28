package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"
)

// `rindler vault` — the on/off switch for credential custody on this machine.
//
// OFF IS THE DEFAULT, and off is a real state, not a label: the machine is not
// paired, so it does not appear under Devices in the dashboard, holds no
// device token, and cannot be asked for a secret by any
// session. Signing in does not turn it on. Storing a credential does not turn it
// on either -- `creds add` writes to a local encrypted file that stays inert
// until someone deliberately enables the vault.
//
// The reason to make this explicit rather than implicit: turning the vault on
// enrolls a laptop as a credential custodian that a remote session can call.
// That is a decision a person should make on purpose, and be able to see and
// undo from either dashboard afterwards.

const vaultDisabledHint = "Credential vault is off on this machine. Turn it on with `rindler vault enable`."

// vaultEnabled reports whether this machine is acting as a credential custodian.
//
// Enrollment IS the switch. There is no separate boolean that could disagree
// with reality: if the machine is paired it can be asked for secrets, and if it
// is not, it cannot. A flag stored beside the pairing could drift out of sync
// with the server's view and produce a vault that reads "off" locally while a
// session is still able to reach it.
func vaultEnabled() bool { return deviceIsPaired() }

func runVault(args []string) int {
	if len(args) == 0 {
		return runVaultStatus()
	}
	switch args[0] {
	case "enable", "on":
		return runVaultEnable(args[1:])
	case "disable", "off":
		return runVaultDisable()
	case "status":
		return runVaultStatus()
	default:
		fmt.Fprintf(os.Stderr, "unknown vault subcommand %q (want enable|disable|status)\n", args[0])
		return 2
	}
}

func runVaultStatus() int {
	if !vaultEnabled() {
		fmt.Println("Credential vault: OFF")
		fmt.Println("  This machine is not paired, so it does not appear under Devices in the")
		fmt.Println("  dashboard, and no session can ask it for a login.")
		if n := storedCredentialCount(); n > 0 {
			// Say this plainly: someone who added credentials expects them to work.
			fmt.Printf("  %d credential(s) are stored locally and encrypted, but INERT until you\n", n)
			fmt.Println("  enable the vault.")
		}
		fmt.Println("\nTurn it on:  rindler vault enable")
		return 0
	}
	d, _ := loadDeviceIdentity()
	fmt.Println("Credential vault: ON")
	fmt.Printf("  paired as %q (device %s)\n", d.DeviceName, d.DeviceID)
	fmt.Printf("  %d credential(s) stored, encrypted on this device\n", storedCredentialCount())
	fmt.Println("  manage or revoke it under Auto Login \u2192 Devices in the dashboard")
	fmt.Println("\nServe requests:  rindler device serve")
	fmt.Println("Turn it off:     rindler vault disable")
	return 0
}

func runVaultEnable(args []string) int {
	fs := flag.NewFlagSet("vault enable", flag.ContinueOnError)
	apiBaseFlag := fs.String("api-base", "", "Rindler API origin")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if vaultEnabled() {
		d, _ := loadDeviceIdentity()
		fmt.Printf("Credential vault is already on (paired as %q).\n", d.DeviceName)
		return 0
	}
	key, apiBase, code := resolveKeyAndBase(*apiBaseFlag, "vault enable")
	if code != 0 {
		return code
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	d, err := pairDevice(ctx, defaultHTTPClient(), apiBase, key)
	if err != nil {
		fmt.Fprintln(os.Stderr, "vault enable:", err)
		return 1
	}
	fmt.Printf("✓ Credential vault ON. Paired as %q.\n", d.DeviceName)
	fmt.Println("  It now appears under Auto Login \u2192 Devices in the dashboard,")
	fmt.Println("  where you can revoke it at any time.")
	fmt.Println("\n  Secrets stay on this machine: each request is verified against the")
	fmt.Println("  server's signing key and answered from your local encrypted vault,")
	fmt.Println("  sealed so Rindler's server cannot read what it relays.")
	fmt.Println("\nNext:  rindler creds add <site> --username <you>")
	fmt.Println("       rindler device serve")
	return 0
}

func runVaultDisable() int {
	if !vaultEnabled() {
		fmt.Println("Credential vault is already off.")
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// Two different failures. A serverRevokeError means the local identity IS
	// erased and only the dashboard row survived, so custody really is off here
	// and the user has one manual step left. Anything else means the erase
	// itself did not finish, which is a hard failure.
	err := unpairDevice(ctx, defaultHTTPClient())
	var revokeErr *serverRevokeError
	switch {
	case errors.As(err, &revokeErr):
		fmt.Println("✓ Credential vault OFF. This machine's device key is erased, so no")
		fmt.Println("  session can get a credential from it.")
		fmt.Fprintf(os.Stderr, "\n⚠ Could not tell the server: %v\n", err)
		fmt.Fprintln(os.Stderr, "  Remove it from the Devices list on your dashboard.")
	case err != nil:
		fmt.Fprintln(os.Stderr, "vault disable:", err)
		return 1
	default:
		fmt.Println("✓ Credential vault OFF. This machine is unpaired and no longer")
		fmt.Println("  reachable from the dashboard.")
	}
	// Deleting the vault is a separate, destructive act. Turning custody off
	// should not throw away credentials the user may want back tomorrow.
	if n := storedCredentialCount(); n > 0 {
		fmt.Printf("\n  %d credential(s) remain stored and encrypted on this machine.\n", n)
		fmt.Println("  Remove them with `rindler creds rm <site>`.")
	}
	return 0
}

// storedCredentialCount reports how many credentials the local vault holds. Used
// only to tell the user whether anything is sitting there inert; it never opens a
// record, so a locked or unreadable vault simply reports 0.
func storedCredentialCount() int {
	vf, err := loadVault()
	if err != nil {
		return 0
	}
	return len(vf.Records)
}
