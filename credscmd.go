// `rindler creds` — manage the on-device credential vault.
//
//	rindler creds add <site> --username <u> [--otp sms|email] [--label L]
//	rindler creds list
//	rindler creds show <site>      (metadata only — never the secret)
//	rindler creds rm <site>
//
// The password is read from a prompt or stdin, never from a flag: an argument
// would land in shell history, `ps` output, and any transcript of this session.

package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

func runCreds(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: rindler creds add|list|show|rm")
		return 2
	}
	switch args[0] {
	case "add":
		return runCredsAdd(args[1:])
	case "list", "ls":
		return runCredsList(args[1:])
	case "show":
		return runCredsShow(args[1:])
	case "rm", "remove", "delete":
		return runCredsRemove(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown creds subcommand %q (want add|list|show|rm)\n", args[0])
		return 2
	}
}

// readSecret takes the password from a TTY prompt when there is one, and from
// stdin otherwise so the command stays scriptable (`echo pw | rindler creds add`).
//
// On a TTY the terminal's echo is turned OFF for the duration of the read, so
// the password never reaches the screen, scrollback, tmux buffers or an
// asciinema/script recording. If echo cannot be disabled we refuse to prompt
// rather than render the secret: a password on screen is not a smaller failure
// than an error message.
func readSecret(prompt string) (string, error) {
	fi, err := os.Stdin.Stat()
	piped := err == nil && (fi.Mode()&os.ModeCharDevice) == 0
	if piped {
		line, rerr := bufio.NewReader(os.Stdin).ReadString('\n')
		if rerr != nil && rerr != io.EOF {
			return "", rerr
		}
		return strings.TrimRight(line, "\r\n"), nil
	}

	fmt.Fprint(os.Stderr, prompt)
	var line string
	rerr := withEchoDisabled(os.Stdin, func() error {
		l, e := bufio.NewReader(os.Stdin).ReadString('\n')
		if e != nil && e != io.EOF {
			return e
		}
		line = l
		return nil
	})
	if errors.Is(rerr, errEchoUnavailable) {
		fmt.Fprintln(os.Stderr)
		return "", fmt.Errorf("cannot hide the password on this terminal; pipe it instead: echo 'password' | rindler creds add <site> --username <user>")
	}
	if rerr != nil {
		return "", rerr
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func runCredsAdd(args []string) int {
	fs := flag.NewFlagSet("creds add", flag.ContinueOnError)
	username := fs.String("username", "", "account username or email for the site")
	label := fs.String("label", "", "optional human label")
	otp := fs.String("otp", "", "second factor this login needs: sms or email")
	rest, err := parseAnyOrder(fs, args)
	if err != nil {
		return 2
	}
	if len(rest) < 1 || strings.TrimSpace(*username) == "" {
		fmt.Fprintln(os.Stderr, "usage: rindler creds add <site> --username <user> [--otp sms|email] [--label L]")
		return 2
	}
	site, err := normalizeVaultSite(rest[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "creds add:", err)
		return 2
	}
	switch *otp {
	case "", "sms", "email":
	default:
		fmt.Fprintf(os.Stderr, "creds add: --otp must be sms or email, got %q\n", *otp)
		return 2
	}

	password, err := readSecret(fmt.Sprintf("Password for %s on %s: ", *username, site))
	if err != nil {
		fmt.Fprintln(os.Stderr, "creds add:", err)
		return 1
	}
	if password == "" {
		fmt.Fprintln(os.Stderr, "creds add: no password entered; nothing stored")
		return 1
	}

	key, warning, err := vaultMasterKey()
	if err != nil {
		fmt.Fprintln(os.Stderr, "creds add:", err)
		return 1
	}
	vf, err := loadVault()
	if err != nil {
		fmt.Fprintln(os.Stderr, "creds add:", err)
		return 1
	}
	nonce, ct, err := vaultSeal(key, site, vaultSecret{Username: *username, Password: password})
	if err != nil {
		fmt.Fprintln(os.Stderr, "creds add:", err)
		return 1
	}
	now := time.Now().UTC().Format(time.RFC3339)
	rec := vaultRecord{
		Site: site, Label: *label, Nonce: nonce, Cipher: ct,
		CreatedAt: now, OTPMethod: *otp,
	}
	if i := findVaultRecord(vf, site); i >= 0 {
		rec.CreatedAt = vf.Records[i].CreatedAt
		rec.UpdatedAt = now
		// Replacing in place: the old ciphertext is overwritten, not appended,
		// so a rotated password leaves no readable predecessor behind.
		vf.Records[i] = rec
	} else {
		vf.Records = append(vf.Records, rec)
	}
	if err := saveVault(vf); err != nil {
		fmt.Fprintln(os.Stderr, "creds add:", err)
		return 1
	}
	if warning != "" {
		fmt.Fprintln(os.Stderr, "note:", warning)
	}
	fmt.Printf("✓ Stored %s for %s (encrypted on this device).\n", *username, site)
	if !vaultEnabled() {
		// Do not let this look finished when it is not. The credential is safe
		// and encrypted, but nothing can use it until custody is switched on,
		// and a silent no-op here would surface much later as a login that
		// mysteriously reports no device holds the credential.
		fmt.Println("  Note: the credential vault is OFF, so nothing can use this yet.")
		fmt.Println("  Turn it on:  rindler vault enable")
	}
	if *otp != "" {
		fmt.Printf("  second factor: %s\n", *otp)
	}
	return 0
}

func runCredsList(args []string) int {
	fs := flag.NewFlagSet("creds list", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	vf, err := loadVault()
	if err != nil {
		fmt.Fprintln(os.Stderr, "creds list:", err)
		return 1
	}
	if len(vf.Records) == 0 {
		fmt.Println("No credentials stored.")
		fmt.Println("Add one:  rindler creds add example.com --username you@example.com")
		return 0
	}
	recs := append([]vaultRecord(nil), vf.Records...)
	sort.Slice(recs, func(i, j int) bool { return recs[i].Site < recs[j].Site })
	fmt.Printf("%-28s %-10s %s\n", "SITE", "2FA", "ADDED")
	for _, r := range recs {
		otp := r.OTPMethod
		if otp == "" {
			otp = "-"
		}
		fmt.Printf("%-28s %-10s %s\n", r.Site, otp, r.CreatedAt)
	}
	fmt.Printf("\n%d credential(s), encrypted on this device. Secrets are never printed.\n", len(recs))
	return 0
}

func runCredsShow(args []string) int {
	fs := flag.NewFlagSet("creds show", flag.ContinueOnError)
	rest, err := parseAnyOrder(fs, args)
	if err != nil {
		return 2
	}
	if len(rest) < 1 {
		fmt.Fprintln(os.Stderr, "usage: rindler creds show <site>")
		return 2
	}
	site, err := normalizeVaultSite(rest[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "creds show:", err)
		return 2
	}
	vf, err := loadVault()
	if err != nil {
		fmt.Fprintln(os.Stderr, "creds show:", err)
		return 1
	}
	i := findVaultRecord(vf, site)
	if i < 0 {
		fmt.Fprintf(os.Stderr, "no credential stored for %s\n", site)
		return 1
	}
	rec := vf.Records[i]

	// Open the record to prove the vault can actually decrypt it, then report the
	// USERNAME only. A vault whose contents can be dumped on demand is readable
	// by anything that can run this binary.
	key, _, err := vaultMasterKey()
	if err != nil {
		fmt.Fprintln(os.Stderr, "creds show:", err)
		return 1
	}
	sec, err := vaultOpen(key, rec)
	if err != nil {
		fmt.Fprintf(os.Stderr, "creds show: %v\n", err)
		return 1
	}
	fmt.Printf("site:     %s\n", rec.Site)
	fmt.Printf("username: %s\n", sec.Username)
	fmt.Printf("password: (stored, %d characters — not printed)\n", len(sec.Password))
	if rec.Label != "" {
		fmt.Printf("label:    %s\n", rec.Label)
	}
	if rec.OTPMethod != "" {
		fmt.Printf("2fa:      %s\n", rec.OTPMethod)
	}
	fmt.Printf("added:    %s\n", rec.CreatedAt)
	if rec.UpdatedAt != "" {
		fmt.Printf("updated:  %s\n", rec.UpdatedAt)
	}
	return 0
}

func runCredsRemove(args []string) int {
	fs := flag.NewFlagSet("creds rm", flag.ContinueOnError)
	rest, err := parseAnyOrder(fs, args)
	if err != nil {
		return 2
	}
	if len(rest) < 1 {
		fmt.Fprintln(os.Stderr, "usage: rindler creds rm <site>")
		return 2
	}
	site, err := normalizeVaultSite(rest[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "creds rm:", err)
		return 2
	}
	vf, err := loadVault()
	if err != nil {
		fmt.Fprintln(os.Stderr, "creds rm:", err)
		return 1
	}
	i := findVaultRecord(vf, site)
	if i < 0 {
		fmt.Fprintf(os.Stderr, "no credential stored for %s\n", site)
		return 1
	}
	vf.Records = append(vf.Records[:i], vf.Records[i+1:]...)
	if err := saveVault(vf); err != nil {
		fmt.Fprintln(os.Stderr, "creds rm:", err)
		return 1
	}
	fmt.Printf("✓ Removed the credential for %s.\n", site)
	return 0
}
