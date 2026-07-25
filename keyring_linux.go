//go:build linux

package main

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
)

// keyringService is the Secret Service attribute under which the rindler CLI key
// is stored (mirrors the macOS Keychain service name).
const keyringService = "rindler-cli"

// linuxKeyring drives the freedesktop Secret Service via `secret-tool`
// (libsecret-tools). set reads the secret from STDIN, so it never appears in the
// process argv. No CGO.
type linuxKeyring struct{}

func newSystemBackend() (keyringBackend, error) {
	if _, err := exec.LookPath("secret-tool"); err != nil {
		return nil, errors.New("`secret-tool` not found (install libsecret-tools)")
	}
	return linuxKeyring{}, nil
}

func (linuxKeyring) set(account, secret string) error {
	cmd := exec.CommandContext(context.Background(), "secret-tool", "store", "--label=Rindler CLI",
		"service", keyringService, "account", account)
	cmd.Stdin = strings.NewReader(secret)
	return cmd.Run()
}

func (linuxKeyring) get(account string) (string, error) {
	out, err := exec.CommandContext(context.Background(), "secret-tool", "lookup",
		"service", keyringService, "account", account).Output()
	if err != nil {
		if ee := (&exec.ExitError{}); errors.As(err, &ee) {
			return "", errNoEntry
		}
		return "", err
	}
	if len(bytes.TrimSpace(out)) == 0 {
		return "", errNoEntry
	}
	return strings.TrimRight(string(out), "\n"), nil
}

func (linuxKeyring) del(account string) error {
	return exec.CommandContext(context.Background(), "secret-tool", "clear",
		"service", keyringService, "account", account).Run()
}
