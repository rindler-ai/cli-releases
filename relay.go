package main

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// The relay loop: this machine holds a socket open to Rindler and answers signed
// requests for exactly one credential at a time, out of the local vault.
//
// This is what makes the CLI an Auto-Login app rather than a credential drawer.
// The credential never leaves the device in the clear -- it is sealed to the
// login worker's per-login key, so Rindler's own server relays a ciphertext it
// cannot open.
//
// Refusal is the default everywhere below. An unsigned ping, an unverifiable
// one, an unknown secret kind, a site the vault does not hold: each is declined,
// never guessed at. A credential released by mistake cannot be un-released.

const (
	relayReconnectMin = 2 * time.Second
	relayReconnectMax = 60 * time.Second
	relayHelloTimeout = 20 * time.Second
	// A relay that goes quiet is reaped by intermediaries long before anything
	// notices, and the symptom is not an error -- it is a login that reports "no
	// device holds this credential" because the socket happened to be down when
	// the server asked. Ping well inside the usual 60s idle windows.
	relayKeepalive = 20 * time.Second
)

// runRelay connects and serves until ctx is cancelled, reconnecting with backoff.
// A dropped socket is normal (laptop sleeps, networks move); the loop treats it
// as such rather than exiting, because a relay that quietly stops is a login
// that mysteriously hangs later.
func runRelay(ctx context.Context, verbose bool) error {
	d, err := loadDeviceIdentity()
	if err != nil || d.DeviceToken == "" {
		return errors.New("this machine is not paired; run `rindler vault enable` first")
	}
	if len(d.ServerPubkey) != ed25519.PublicKeySize {
		// Fail closed. Without the server's key we cannot tell a real request
		// from an attacker's, and serving credentials on unverifiable pings is
		// precisely the failure this whole design exists to prevent.
		return errors.New("this device paired against a lane with no relay signing key; run `rindler vault disable` then `rindler vault enable` to re-pair before serving credentials")
	}

	backoff := relayReconnectMin
	for {
		if ctx.Err() != nil {
			return nil
		}
		err := relaySession(ctx, d, verbose)
		if ctx.Err() != nil {
			return nil
		}
		if verbose && err != nil {
			fmt.Fprintf(os.Stderr, "relay: disconnected (%v); reconnecting in %s\n", err, backoff)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > relayReconnectMax {
			backoff = relayReconnectMax
		}
	}
}

func relaySession(ctx context.Context, d deviceIdentity, verbose bool) error {
	wsURL := strings.TrimRight(d.APIBase, "/") + "/v1/devices/connect"
	wsURL = strings.Replace(strings.Replace(wsURL, "https://", "wss://", 1), "http://", "ws://", 1)

	c, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPClient: &http.Client{}})
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer c.CloseNow()
	// Credentials are small; a generous cap still refuses an unbounded frame.
	c.SetReadLimit(1 << 20)

	hello := relayWire{Type: "hello", Token: d.DeviceToken, V: relayProtocolVersion}
	if err := writeWire(ctx, c, hello); err != nil {
		return fmt.Errorf("hello: %w", err)
	}
	hctx, cancel := context.WithTimeout(ctx, relayHelloTimeout)
	defer cancel()
	var ack relayWire
	if err := readWire(hctx, c, &ack); err != nil {
		return fmt.Errorf("hello reply: %w", err)
	}
	if ack.Type != "hello_ok" || ack.Error != "" {
		return fmt.Errorf("server refused this device: %s", firstNonEmpty(ack.Error, ack.Type))
	}
	if verbose {
		fmt.Fprintf(os.Stderr, "relay: connected as device %s\n", ack.DeviceID)
	}

	// Keepalive. Without it the socket is idle between logins -- which is almost
	// all of the time -- and gets dropped; the reconnect backoff then leaves
	// windows where the device is paired but unreachable.
	pingCtx, stopPing := context.WithCancel(ctx)
	defer stopPing()
	go func() {
		t := time.NewTicker(relayKeepalive)
		defer t.Stop()
		for {
			select {
			case <-pingCtx.Done():
				return
			case <-t.C:
				pctx, cancel := context.WithTimeout(pingCtx, 10*time.Second)
				err := c.Ping(pctx)
				cancel()
				if err != nil {
					// The read loop will surface the closure; nothing to do here.
					return
				}
			}
		}
	}()

	for {
		var msg relayWire
		if err := readWire(ctx, c, &msg); err != nil {
			return err
		}
		switch msg.Type {
		case "ping":
			if msg.Ping == nil {
				continue
			}
			handlePing(ctx, c, d, *msg.Ping, verbose)
		case "inventory":
			reqID := msg.RequestID
			if msg.Inventory != nil && msg.Inventory.RequestID != "" {
				reqID = msg.Inventory.RequestID
			}
			handleInventory(ctx, c, reqID)
		case "ack", "heartbeat", "hello_ok":
			// Nothing to do; the server is confirming or keeping the pipe warm.
		default:
			// An unknown frame is ignored rather than fatal: a newer server must
			// be able to add message types without bricking older clients.
		}
	}
}

// handlePing answers one credential request, or declines it.
func handlePing(ctx context.Context, c *websocket.Conn, d deviceIdentity, p secretPing, verbose bool) {
	decline := func(reason string) {
		if verbose {
			fmt.Fprintf(os.Stderr, "relay: declined %s for %s: %s\n", p.SecretKind, p.Site, reason)
		}
		_ = writeWire(ctx, c, relayWire{Type: "declined", RequestID: p.RequestID})
	}

	// Verify BEFORE touching the vault. Reading a secret we are not going to
	// release still pulls it into memory for no reason.
	if !verifyPingSignature(ed25519.PublicKey(d.ServerPubkey), p) {
		decline("signature did not verify")
		return
	}
	if !validSecretKind(p.SecretKind) {
		decline("this device does not serve " + string(p.SecretKind))
		return
	}

	site, err := normalizeVaultSite(p.Site)
	if err != nil {
		decline("unusable site")
		return
	}
	key, _, err := vaultMasterKey()
	if err != nil {
		decline("vault unavailable")
		return
	}
	vf, err := loadVault()
	if err != nil {
		decline("vault unreadable")
		return
	}
	idx := findVaultRecord(vf, site)
	if idx < 0 {
		decline("no credential stored for " + site)
		return
	}
	sec, err := vaultOpen(key, vf.Records[idx])
	if err != nil {
		decline("could not decrypt the stored credential")
		return
	}

	var plain string
	switch p.SecretKind {
	case secretUsername:
		plain = sec.Username
	case secretPassword:
		plain = sec.Password
	}
	if plain == "" {
		decline("stored credential has no " + string(p.SecretKind))
		return
	}

	sealed, err := sealToWorker(p.WorkerEphemeralPubkey, sealInfo(p.RequestID, p.Site, p.SecretKind), []byte(plain))
	if err != nil {
		decline("seal failed")
		return
	}
	sig := ed25519.Sign(ed25519.PrivateKey(d.PrivateKey),
		releaseSigningMessage(p.RequestID, p.Challenge, p.WorkerEphemeralPubkey, sealed))

	if verbose {
		// Names the site and kind, never the value.
		fmt.Fprintf(os.Stderr, "relay: released %s for %s\n", p.SecretKind, site)
	}
	_ = writeWire(ctx, c, relayWire{
		Type:      "release",
		RequestID: p.RequestID,
		Release:   &secretRelease{RequestID: p.RequestID, SealedSecret: sealed, DeviceSignature: sig},
	})
}

// handleInventory reports which sites this device can serve. Domains only: the
// server uses it to route a login here, and it has no business knowing the
// usernames.
func handleInventory(ctx context.Context, c *websocket.Conn, requestID string) {
	// Advertise CANONICAL domains. The server matches this list with its own
	// normalizeDomain and then pings using the matched entry, so sending a
	// verbatim "www." host makes it ping for a name our own store does not use.
	// Canonicalising here means the domain it pings is the domain we look up.
	domains := []string{}
	if vf, err := loadVault(); err == nil {
		seen := map[string]bool{}
		for _, r := range vf.Records {
			d := canonicalVaultSite(r.Site)
			// An older vault can hold both "example.com" and "www.example.com";
			// they canonicalise to one domain and the server should see one.
			if d == "" || seen[d] {
				continue
			}
			seen[d] = true
			domains = append(domains, d)
		}
	}
	_ = writeWire(ctx, c, relayWire{
		Type:      "inventory_reply",
		RequestID: requestID,
		InventoryReply: &siteInventoryReply{
			RequestID: requestID,
			Domains:   domains,
		},
	})
}

func writeWire(ctx context.Context, c *websocket.Conn, v relayWire) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.Write(ctx, websocket.MessageText, b)
}

func readWire(ctx context.Context, c *websocket.Conn, out *relayWire) error {
	_, b, err := c.Read(ctx)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}
