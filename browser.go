package main

import (
	"context"
	"os"
	"os/exec"
	"runtime"
)

// openBrowser attempts to open url in the user's default browser. It returns an
// error if it cannot launch a browser (headless / SSH / no opener), so the caller
// can fall back to printing the URL for manual opening.
//
// browserOpener is a package var so tests can stub it.
var browserOpener = openBrowser

func openBrowser(url string) error {
	// Fire-and-forget launch; a background context is appropriate (we do not want
	// to kill the browser when the CLI's login context times out).
	ctx := context.Background()
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.CommandContext(ctx, "open", url)
	case "windows":
		cmd = exec.CommandContext(ctx, "rundll32", "url.dll,FileProtocolHandler", url)
	default: // linux and friends
		cmd = exec.CommandContext(ctx, "xdg-open", url)
	}
	return cmd.Start()
}

// browserLikelyAvailable is a heuristic: a browser is unlikely to open on a
// headless box (no display) or over SSH, so login should default to the paste
// flow there.
func browserLikelyAvailable() bool {
	if os.Getenv("SSH_CONNECTION") != "" || os.Getenv("SSH_TTY") != "" {
		return false
	}
	switch runtime.GOOS {
	case "darwin", "windows":
		return true
	default:
		// On Linux a GUI needs a display server.
		return os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""
	}
}
