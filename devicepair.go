package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Device enrollment: the CLI pairs as a custody device, exactly as the Auto-Login
// app does, so it shows up in the dashboard under Auto Login and can be
// revoked from either.
//
// The point is not cosmetic. A machine holding a credential vault must be
// revocable from the account that owns it; before this, the CLI held credentials
// with no device-level off switch anywhere in the product.

// deviceIdentity is this machine's enrollment. It lives in its own file, NOT in
// credentials.json, for the same reason the vault master key does: `rindler
// logout` clears the session, and it must not silently unpair the machine or
// destroy the private key that identifies it.
type deviceIdentity struct {
	DeviceID string `json:"device_id"`
	// DeviceToken authenticates this device on the relay socket. It is a bearer
	// token, so the file is 0600 and it is never printed.
	DeviceToken string `json:"device_token"`
	// PrivateKey is the Ed25519 private half whose public half the server holds.
	// Signing releases with it is what proves a sealed secret came from THIS
	// device rather than from anyone who reached the socket.
	PrivateKey []byte `json:"private_key"`
	// ServerPubkey verifies every incoming SecretPing. Empty means the lane had
	// no signing key configured at pair time, and the client fails closed rather
	// than serving secrets on unverifiable requests.
	ServerPubkey []byte `json:"server_pubkey"`
	DeviceName   string `json:"device_name"`
	Platform     string `json:"platform"`
	APIBase      string `json:"api_base"`
	PairedAt     string `json:"paired_at"`
}

func deviceIdentityPath() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "device.json"), nil
}

func loadDeviceIdentity() (deviceIdentity, error) {
	var d deviceIdentity
	p, err := deviceIdentityPath()
	if err != nil {
		return d, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return d, err
	}
	if err := json.Unmarshal(b, &d); err != nil {
		return d, fmt.Errorf("device.json is corrupt: %w", err)
	}
	return d, nil
}

func saveDeviceIdentity(d deviceIdentity) error {
	p, err := deviceIdentityPath()
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	return writeFilePreservePerm(p, b, 0o600)
}

func deviceIsPaired() bool {
	d, err := loadDeviceIdentity()
	return err == nil && d.DeviceID != "" && d.DeviceToken != ""
}

// defaultDeviceName names the row a human will read on the dashboard. The
// hostname is the only thing that reliably distinguishes one of a user's
// machines from another, which is the whole job of this field.
func defaultDeviceName() string {
	h, err := os.Hostname()
	if err != nil || strings.TrimSpace(h) == "" {
		return "rindler CLI"
	}
	return fmt.Sprintf("%s (CLI)", strings.TrimSpace(h))
}

// devicePlatform maps GOOS onto the platform vocabulary the server stores.
// Unknown values pass through rather than being forced into a wrong bucket: a
// row reading "freebsd" is honest, one reading "linux" would be a lie.
func devicePlatform() string {
	switch runtime.GOOS {
	case "darwin":
		return "macos"
	case "windows":
		return "windows"
	case "linux":
		return "linux"
	default:
		return runtime.GOOS
	}
}

// pairDevice enrolls this machine. It mints a pairing token through the
// Clerk-authed endpoint using the session the CLI already holds, then redeems it
// device-side -- the same two-step the app performs, so the server path is
// identical and nothing CLI-specific is trusted.
func pairDevice(ctx context.Context, httpc *http.Client, apiBase, sessionKey string) (deviceIdentity, error) {
	var out deviceIdentity

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return out, fmt.Errorf("generate device key: %w", err)
	}
	name := defaultDeviceName()

	initBody, _ := json.Marshal(map[string]string{"device_name": name})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(apiBase, "/")+"/v1/devices/pair/init", bytes.NewReader(initBody))
	if err != nil {
		return out, err
	}
	req.Header.Set("Authorization", "Bearer "+sessionKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpc.Do(req)
	if err != nil {
		return out, fmt.Errorf("pair/init: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("pair/init: %s", errBody(resp))
	}
	var initOut struct {
		PairingToken string `json:"pairing_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&initOut); err != nil {
		return out, fmt.Errorf("pair/init: bad response: %w", err)
	}
	if initOut.PairingToken == "" {
		return out, fmt.Errorf("pair/init: server returned no pairing token")
	}

	completeBody, _ := json.Marshal(map[string]string{
		"pairing_token": initOut.PairingToken,
		"device_name":   name,
		"platform":      devicePlatform(),
		"device_pubkey": base64.StdEncoding.EncodeToString(pub),
		"client_kind":   "cli",
	})
	req2, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(apiBase, "/")+"/devices/pair/complete", bytes.NewReader(completeBody))
	if err != nil {
		return out, err
	}
	req2.Header.Set("Content-Type", "application/json")
	resp2, err := httpc.Do(req2)
	if err != nil {
		return out, fmt.Errorf("pair/complete: %w", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		return out, fmt.Errorf("pair/complete: %s", errBody(resp2))
	}
	var doneOut struct {
		DeviceID     string `json:"device_id"`
		DeviceToken  string `json:"device_token"`
		ServerPubkey string `json:"server_pubkey"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&doneOut); err != nil {
		return out, fmt.Errorf("pair/complete: bad response: %w", err)
	}
	if doneOut.DeviceID == "" || doneOut.DeviceToken == "" {
		return out, fmt.Errorf("pair/complete: server returned no device identity")
	}
	serverPub, _ := base64.StdEncoding.DecodeString(doneOut.ServerPubkey)

	out = deviceIdentity{
		DeviceID:     doneOut.DeviceID,
		DeviceToken:  doneOut.DeviceToken,
		PrivateKey:   priv,
		ServerPubkey: serverPub,
		DeviceName:   name,
		Platform:     devicePlatform(),
		APIBase:      strings.TrimRight(apiBase, "/"),
		PairedAt:     time.Now().UTC().Format(time.RFC3339),
	}
	return out, saveDeviceIdentity(out)
}

// unpairDevice revokes this device server-side using its OWN bearer token, then
// removes the local identity. Called by `rindler logout` so signing out of a
// machine also retires it, instead of leaving an orphan row that still looks
// pairable on the dashboard.
//
// Best-effort by design: if the server call fails, the local identity is still
// removed. Leaving a private key and device token behind on a machine the user
// asked to sign out is the worse outcome; the stale row can be revoked from the
// dashboard, but an un-erased key cannot be recalled.
func unpairDevice(ctx context.Context, httpc *http.Client) error {
	d, err := loadDeviceIdentity()
	if err != nil {
		return nil // never paired; nothing to do
	}
	if d.APIBase != "" && d.DeviceToken != "" {
		req, rerr := http.NewRequestWithContext(ctx, http.MethodPost, d.APIBase+"/devices/revoke-self", nil)
		if rerr == nil {
			req.Header.Set("Authorization", "Bearer "+d.DeviceToken)
			if resp, derr := httpc.Do(req); derr == nil {
				resp.Body.Close()
			}
		}
	}
	p, perr := deviceIdentityPath()
	if perr != nil {
		return perr
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// errBody renders a failed response for a human without dumping an unbounded
// body into the terminal.
func errBody(resp *http.Response) string {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	msg := strings.TrimSpace(string(b))
	if msg == "" {
		return resp.Status
	}
	return fmt.Sprintf("%s: %s", resp.Status, msg)
}
