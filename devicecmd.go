package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"
	"time"
)

// `rindler device` — pair this machine, see what is paired, unpair it, and run
// the relay that serves stored credentials to a session.

func runDevice(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: rindler device <pair|status|list|unpair|serve>")
		return 2
	}
	switch args[0] {
	case "pair":
		// One switch, one vocabulary: pairing IS enabling the vault.
		return runVaultEnable(args[1:])
	case "status":
		return runDeviceStatus()
	case "list":
		return runDeviceList(args[1:])
	case "unpair":
		return runVaultDisable()
	case "serve", "relay":
		return runDeviceServe(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown device subcommand %q (want pair|status|list|unpair|serve)\n", args[0])
		return 2
	}
}

func runDeviceStatus() int {
	d, err := loadDeviceIdentity()
	if err != nil || d.DeviceID == "" {
		fmt.Println("This machine is not paired.")
		fmt.Println("Pair it:  rindler device pair")
		return 0
	}
	fmt.Printf("device:   %s\n", d.DeviceID)
	fmt.Printf("name:     %s\n", d.DeviceName)
	fmt.Printf("platform: %s (paired as a CLI device)\n", d.Platform)
	fmt.Printf("paired:   %s\n", d.PairedAt)
	// Report the signing key as present/absent, never its bytes. Absent is the
	// one case that changes behavior: the relay refuses to serve without it.
	if len(d.ServerPubkey) == 0 {
		fmt.Println("relay:    NOT USABLE — no server signing key was issued at pairing")
		fmt.Println("          re-pair against a lane that has one before serving credentials")
	} else {
		fmt.Println("relay:    ready (server signing key present)")
	}
	return 0
}

func runDeviceList(args []string) int {
	fs := flag.NewFlagSet("device list", flag.ContinueOnError)
	apiBaseFlag := fs.String("api-base", "", "Rindler API origin")
	jsonOut := fs.Bool("json", false, "print the raw JSON list")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	key, apiBase, code := resolveKeyAndBase(*apiBaseFlag, "device list")
	if code != 0 {
		return code
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBase+"/v1/devices", nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "device list:", err)
		return 1
	}
	req.Header.Set("Authorization", "Bearer "+key)
	res, err := defaultHTTPClient().Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "device list:", err)
		return 1
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if res.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "device list:", verbError("device list", res.StatusCode, string(body)))
		return 1
	}
	if *jsonOut {
		fmt.Println(string(body))
		return 0
	}
	var out struct {
		Devices []struct {
			ID         string `json:"id"`
			DeviceName string `json:"device_name"`
			Platform   string `json:"platform"`
			ClientKind string `json:"client_kind"`
			Status     string `json:"status"`
			LastSeenAt string `json:"last_seen_at"`
		} `json:"devices"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		fmt.Fprintln(os.Stderr, "device list: unreadable response")
		return 1
	}
	if len(out.Devices) == 0 {
		fmt.Println("No devices paired to this account.")
		return 0
	}
	mine, _ := loadDeviceIdentity()
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tKIND\tPLATFORM\tSTATUS\tLAST SEEN")
	for _, d := range out.Devices {
		kind := d.ClientKind
		if kind == "" {
			kind = "app"
		}
		name := d.DeviceName
		if d.ID != "" && d.ID == mine.DeviceID {
			name += "  (this machine)"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", name, kind, d.Platform, d.Status, d.LastSeenAt)
	}
	w.Flush()
	return 0
}

func runDeviceServe(args []string) int {
	fs := flag.NewFlagSet("device serve", flag.ContinueOnError)
	quiet := fs.Bool("quiet", false, "only report errors")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if !vaultEnabled() {
		fmt.Fprintln(os.Stderr, "device serve:", vaultDisabledHint)
		return 1
	}
	// Ctrl-C should stop serving promptly: this process is holding the door open
	// for credential requests, so a lingering one after the user asked it to stop
	// is exactly the wrong behavior.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if !*quiet {
		d, _ := loadDeviceIdentity()
		fmt.Printf("Serving stored credentials for this machine (%s).\n", d.DeviceName)
		fmt.Println("Each request is verified against the server's signing key and answered")
		fmt.Println("from your local vault; secrets are sealed to the login worker and are")
		fmt.Println("never readable by Rindler's server. Press Ctrl-C to stop.")
	}
	if err := runRelay(ctx, !*quiet); err != nil {
		fmt.Fprintln(os.Stderr, "device serve:", err)
		return 1
	}
	if !*quiet {
		fmt.Println("\nStopped serving.")
	}
	return 0
}
