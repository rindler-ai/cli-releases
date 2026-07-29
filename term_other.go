//go:build !linux && !darwin

package main

import "os"

// withEchoDisabled has no termios on this platform. Fail closed rather than
// echoing a secret: the caller falls back to refusing the interactive prompt.
func withEchoDisabled(_ *os.File, _ func() error) error {
	return errEchoUnavailable
}
