//go:build darwin

package main

import (
	"context"
	"errors"
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

func (macKeyring) set(account, secret string) error {
	// KNOWN LIMITATION (macOS only): the secret is on argv here (-w); macOS
	// `security add-generic-password` has no reliable non-argv password input
	// without the Keychain Services API via cgo, which this no-cgo backend avoids.
	// -U updates an existing item instead of erroring.
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
