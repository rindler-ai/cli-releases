//go:build linux || darwin

package main

import (
	"os"
	"syscall"
	"unsafe"
)

// Terminal echo control for secret prompts. Stdlib-only by repo policy, so this
// is a direct termios ioctl rather than golang.org/x/term. syscall.Termios and
// the TCGETS/TIOCGETA request numbers are both defined per-platform by the
// stdlib, so the only per-OS part is which request constant to use
// (term_ioctl_*.go).

// withEchoDisabled runs fn with terminal echo turned off on fd, restoring the
// previous state afterwards. If fd is not a terminal (piped input, no TTY) it
// runs fn unchanged: there is no echo to suppress and no state to restore.
func withEchoDisabled(f *os.File, fn func() error) error {
	fd := f.Fd()

	var oldState syscall.Termios
	if _, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, fd, ioctlReadTermios,
		uintptr(unsafe.Pointer(&oldState)), 0, 0, 0); errno != 0 {
		// Not a terminal (or no termios): nothing to disable.
		return fn()
	}

	newState := oldState
	newState.Lflag &^= syscall.ECHO
	// Keep ECHONL so the user's Enter still moves to the next line.
	newState.Lflag |= syscall.ECHONL
	if _, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, fd, ioctlWriteTermios,
		uintptr(unsafe.Pointer(&newState)), 0, 0, 0); errno != 0 {
		// Could not disable echo; fail closed rather than echoing the secret.
		return errEchoUnavailable
	}
	defer func() {
		_, _, _ = syscall.Syscall6(syscall.SYS_IOCTL, fd, ioctlWriteTermios,
			uintptr(unsafe.Pointer(&oldState)), 0, 0, 0)
	}()

	return fn()
}
