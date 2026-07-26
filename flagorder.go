// Flag parsing that tolerates flags AFTER positional arguments.
//
// Go's flag package stops at the first non-flag token, so `rindler actions
// example.com --api-base https://…` silently DROPS --api-base and everything
// after it. That is not a cosmetic problem: the dropped flag falls back to the
// default API origin, so the command quietly talks to production instead of the
// host you named, and reports whatever that host says. It was found exactly that
// way — a dev-lane call answered with a prod 401.
//
// Every verb that takes a positional (`actions <site>`, `map <url>`,
// `run status <job-id>`) goes through parseAnyOrder instead of fs.Parse.

package main

import (
	"flag"
	"strings"
)

// boolFlag is the interface the flag package uses to mark a flag that takes no
// value (so `--json foo` does not swallow `foo` as its argument).
type boolFlag interface {
	IsBoolFlag() bool
}

// takesValue reports whether -name consumes the following token.
func takesValue(fs *flag.FlagSet, name string) bool {
	f := fs.Lookup(name)
	if f == nil {
		// Unknown flag: let flag.Parse produce the real error rather than guessing
		// at its arity here.
		return false
	}
	if bf, ok := f.Value.(boolFlag); ok && bf.IsBoolFlag() {
		return false
	}
	return true
}

// parseAnyOrder splits args into flags and positionals, parses the flags, and
// returns the positionals in their original order. `--` ends flag processing,
// as usual: everything after it is positional.
func parseAnyOrder(fs *flag.FlagSet, args []string) ([]string, error) {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			positional = append(positional, args[i+1:]...)
			i = len(args)
		case strings.HasPrefix(a, "-") && a != "-":
			flags = append(flags, a)
			name := strings.TrimLeft(a, "-")
			// --name=value carries its own value; --name value does not.
			if !strings.Contains(a, "=") && takesValue(fs, name) && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
		default:
			positional = append(positional, a)
		}
	}
	if err := fs.Parse(flags); err != nil {
		return nil, err
	}
	return positional, nil
}
