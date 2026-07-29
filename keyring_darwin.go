//go:build darwin

package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// keyringService is the macOS Keychain service under which the rindler CLI key is
// stored.
const keyringService = "rindler-cli"

// macKeyring drives the macOS login Keychain via the built-in `security` tool.
// No CGO.
type macKeyring struct{}

func newSystemBackend() (keyringBackend, error) {
	if _, err := exec.LookPath("security"); err != nil {
		return nil, errors.New("macOS `security` tool not found on PATH")
	}
	return macKeyring{}, nil
}

// securityQuote wraps a value for `security -i`'s own command parser, which
// splits on whitespace and honors double quotes with backslash escapes.
func securityQuote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(s) + `"`
}

func (macKeyring) set(account, secret string) error {
	// Keep the secret OFF argv. Passing it as `-w <secret>` put the live MCP key
	// (and the vault master key) in the argv of a `security` process, readable by
	// any other process of the same user via ps for the duration of the call.
	// `security -i` reads its command from STDIN instead, so ps sees only
	// "security -i".
	cmd := exec.CommandContext(context.Background(), "security", "-i")
	cmd.Stdin = strings.NewReader(fmt.Sprintf("add-generic-password -U -s %s -a %s -w %s\n",
		securityQuote(keyringService), securityQuote(account), securityQuote(secret)))
	if err := runQuiet(cmd); err == nil {
		return nil
	}
	// Fall back to the argv form rather than fail the login outright: an older or
	// differently-built `security` that cannot do -i should still be able to store
	// the key. Less private, but working beats broken.
	return runQuiet(exec.CommandContext(context.Background(), "security", "add-generic-password",
		"-U", "-s", keyringService, "-a", account, "-w", secret))
}

func (macKeyring) get(account string) (string, error) {
	out, err := exec.CommandContext(context.Background(), "security", "find-generic-password",
		"-s", keyringService, "-a", account, "-w").Output()
	if err != nil {
		if ee := (&exec.ExitError{}); errors.As(err, &ee) {
			return "", errNoEntry
		}
		return "", err
	}
	return strings.TrimRight(string(out), "\n"), nil
}

func (macKeyring) del(account string) error {
	err := runQuiet(exec.CommandContext(context.Background(), "security", "delete-generic-password",
		"-s", keyringService, "-a", account))
	if err != nil {
		if ee := (&exec.ExitError{}); errors.As(err, &ee) {
			return errNoEntry
		}
		return err
	}
	return nil
}

func runQuiet(cmd *exec.Cmd) error {
	cmd.Stdout, cmd.Stderr = nil, nil
	return cmd.Run()
}
