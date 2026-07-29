package main

import "errors"

// errEchoUnavailable means the terminal's echo could not be turned off, so a
// secret prompt would render the password on screen. Callers must refuse to
// prompt rather than leak it into scrollback.
var errEchoUnavailable = errors.New("cannot disable terminal echo")
