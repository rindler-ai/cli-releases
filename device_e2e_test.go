package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Dispatch-level coverage for the credential-custody surface: `vault`, `device`,
// and the pairing that both sit on.
//
// These went in because the whole surface had ZERO tests that ever called it.
// Everything here goes through run([]string{...}) rather than calling handlers
// directly, because dispatch is exactly where the wiring bugs live: a handler
// can be perfect while the switch routes to the wrong one, and a direct call
// would never notice.

// fakeDeviceAPI serves the two-hop pairing flow plus the device list. Pairing is
// deliberately two calls against DIFFERENT path shapes -- /v1/devices/pair/init
// is prefixed and Clerk/CLI-authed, /devices/pair/complete is NOT prefixed and
// is authenticated by the pairing token itself. That asymmetry is real, and a
// fake that smoothed it over would hide a whole class of URL bug.
func fakeDeviceAPI(t *testing.T) (*httptest.Server, ed25519.PublicKey) {
	t.Helper()
	serverPub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("server key: %v", err)
	}
	mux := http.NewServeMux()

	mux.HandleFunc("POST /v1/devices/pair/init", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"pairing_token":"pt-1","expires_in_seconds":600}`))
	})
	mux.HandleFunc("POST /devices/pair/complete", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["pairing_token"] != "pt-1" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		// The client must send its own public key and identify as the CLI.
		if body["device_pubkey"] == "" || body["client_kind"] != "cli" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"device_id":     "dev-1",
			"device_token":  "dt-1",
			"server_pubkey": base64.StdEncoding.EncodeToString(serverPub),
		})
	})
	mux.HandleFunc("POST /devices/revoke-self", func(w http.ResponseWriter, r *http.Request) {
		// Must present the DEVICE token, not the session key.
		if r.Header.Get("Authorization") != "Bearer dt-1" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /v1/devices", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"devices":[
		  {"id":"dev-1","device_name":"box (CLI)","platform":"linux","client_kind":"cli","status":"active"},
		  {"id":"dev-2","device_name":"Pixel","platform":"android","client_kind":"app","status":"active"}]}`))
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, serverPub
}

// OFF is the default and must survive a sign-in. Pairing enrolls this machine as
// a credential custodian a remote session can call, so it has to be a deliberate
// act rather than a side effect of `rindler login`.
func TestVaultIsOffUntilEnabled(t *testing.T) {
	isolate(t)
	if code := run([]string{"vault"}); code != 0 {
		t.Fatalf("bare `vault` should report status and exit 0, got %d", code)
	}
	if vaultEnabled() {
		t.Fatal("a fresh machine must have custody OFF")
	}
	if code := run([]string{"vault", "status"}); code != 0 {
		t.Fatalf("vault status should exit 0, got %d", code)
	}
}

// The highest-value gap in the whole matrix: pairing had no test at all, and it
// is what turns a laptop into something a session can ask for a password.
func TestVaultEnableThroughDispatch(t *testing.T) {
	isolate(t)
	srv, serverPub := fakeDeviceAPI(t)
	t.Setenv("RINDLER_API_KEY", "rindler_live_test")

	if code := run([]string{"vault", "enable", "--api-base", srv.URL}); code != 0 {
		t.Fatalf("vault enable should exit 0, got %d", code)
	}
	if !vaultEnabled() {
		t.Fatal("custody must be ON after enable")
	}

	d, err := loadDeviceIdentity()
	if err != nil {
		t.Fatalf("device identity: %v", err)
	}
	if d.DeviceID != "dev-1" || d.DeviceToken != "dt-1" {
		t.Fatalf("identity not persisted: id=%q token-empty=%v", d.DeviceID, d.DeviceToken == "")
	}
	if len(d.PrivateKey) != ed25519.PrivateKeySize {
		t.Fatalf("private key is %d bytes, want %d", len(d.PrivateKey), ed25519.PrivateKeySize)
	}
	// Without the server's key the relay cannot tell a real request from an
	// attacker's, so a pairing that loses it is worse than no pairing.
	if string(d.ServerPubkey) != string(serverPub) {
		t.Fatal("the server relay signing key was not stored")
	}
	if d.Platform == "" {
		t.Fatal("platform must be recorded on the identity")
	}

	// The identity file holds a private key and a bearer token.
	p, _ := deviceIdentityPath()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat device.json: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("device.json mode is %04o, want 0600", perm)
	}

	// Re-enabling must not mint a second row for one machine.
	if code := run([]string{"vault", "enable", "--api-base", srv.URL}); code != 0 {
		t.Fatalf("re-enable should be a 0-exit no-op, got %d", code)
	}
}

// Turning custody off must actually revoke server-side AND keep the vault: a
// credential lost as a side effect of flipping a switch is not recoverable.
func TestVaultDisableRevokesButKeepsCredentials(t *testing.T) {
	isolate(t)
	srv, _ := fakeDeviceAPI(t)
	t.Setenv("RINDLER_API_KEY", "rindler_live_test")
	if code := run([]string{"vault", "enable", "--api-base", srv.URL}); code != 0 {
		t.Fatal("enable failed")
	}

	key, _, err := vaultMasterKey()
	if err != nil {
		t.Fatalf("master key: %v", err)
	}
	nonce, cipher, err := vaultSeal(key, "example.com", vaultSecret{Username: "u", Password: "p"})
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if err := saveVault(vaultFile{Version: 1, Records: []vaultRecord{{
		Site: "example.com", Nonce: nonce, Cipher: cipher, CreatedAt: "now"}}}); err != nil {
		t.Fatalf("save vault: %v", err)
	}

	if code := run([]string{"vault", "disable"}); code != 0 {
		t.Fatalf("vault disable should exit 0, got %d", code)
	}
	if vaultEnabled() {
		t.Fatal("custody must be OFF after disable")
	}
	if _, err := loadDeviceIdentity(); err == nil {
		t.Fatal("device.json must be gone after disable")
	}
	if got := storedCredentialCount(); got != 1 {
		t.Fatalf("credentials after disable = %d, want 1 (kept on purpose)", got)
	}
	// A second disable is a no-op, not an error: someone checking the switch
	// should not be told something went wrong.
	if code := run([]string{"vault", "disable"}); code != 0 {
		t.Fatalf("second disable should exit 0, got %d", code)
	}
}

// `logout` must retire the device too. Leaving a signed-out machine paired means
// the dashboard keeps offering to route logins to it.
func TestLogoutUnpairsTheDevice(t *testing.T) {
	isolate(t)
	srv, _ := fakeDeviceAPI(t)
	t.Setenv("RINDLER_API_KEY", "rindler_live_test")
	if code := run([]string{"vault", "enable", "--api-base", srv.URL}); code != 0 {
		t.Fatal("enable failed")
	}
	key, _, _ := vaultMasterKey()
	nonce, cipher, _ := vaultSeal(key, "example.com", vaultSecret{Username: "u", Password: "p"})
	_ = saveVault(vaultFile{Version: 1, Records: []vaultRecord{{
		Site: "example.com", Nonce: nonce, Cipher: cipher, CreatedAt: "now"}}})

	if code := run([]string{"logout"}); code != 0 {
		t.Fatalf("logout should exit 0, got %d", code)
	}
	if _, err := loadDeviceIdentity(); err == nil {
		t.Fatal("logout must remove the device identity")
	}
	// The vault is NOT collateral. Its master key lives outside credentials.json
	// precisely so logout cannot orphan it.
	if got := storedCredentialCount(); got != 1 {
		t.Fatalf("credentials after logout = %d, want 1 (must survive)", got)
	}
}

func TestDeviceListThroughDispatch(t *testing.T) {
	isolate(t)
	srv, _ := fakeDeviceAPI(t)
	t.Setenv("RINDLER_API_KEY", "rindler_live_test")
	for _, args := range [][]string{
		{"device", "list", "--api-base", srv.URL},
		{"device", "list", "--json", "--api-base", srv.URL},
		{"device", "status"},
	} {
		if code := run(args); code != 0 {
			t.Errorf("%v should exit 0, got %d", args, code)
		}
	}
}

func TestDeviceUsageAndUnknownSubcommand(t *testing.T) {
	isolate(t)
	if code := run([]string{"device"}); code != 2 {
		t.Errorf("bare `device` should exit 2, got %d", code)
	}
	if code := run([]string{"device", "not-a-thing"}); code != 2 {
		t.Errorf("unknown device subcommand should exit 2, got %d", code)
	}
	if code := run([]string{"vault", "not-a-thing"}); code != 2 {
		t.Errorf("unknown vault subcommand should exit 2, got %d", code)
	}
}

// Serving is the capability that matters. It must refuse while custody is off,
// and the refusal must name the command that actually fixes it -- the message
// said `rindler login` long after pairing moved to `vault enable`.
func TestServeRefusesAndNamesTheRightFix(t *testing.T) {
	isolate(t)
	if code := run([]string{"device", "serve"}); code == 0 {
		t.Fatal("device serve must refuse while custody is off")
	}
	if !strings.Contains(vaultDisabledHint, "vault enable") {
		t.Fatalf("the refusal must name `vault enable`, got %q", vaultDisabledHint)
	}
	if strings.Contains(vaultDisabledHint, "rindler login") {
		t.Fatal("the refusal must not send users to `rindler login`; it does not pair")
	}
}

// A device paired against a lane that issued no signing key cannot verify a
// ping, so it must refuse to serve at all rather than trust unverifiable
// requests.
func TestRelayFailsClosedWithoutAServerSigningKey(t *testing.T) {
	isolate(t)
	dir, _ := configDir()
	_ = os.MkdirAll(dir, 0o700)
	_ = saveDeviceIdentity(deviceIdentity{
		DeviceID: "dev-1", DeviceToken: "dt-1",
		PrivateKey: make([]byte, ed25519.PrivateKeySize),
		APIBase:    "https://example.invalid",
		// ServerPubkey deliberately absent.
	})
	err := runRelay(t.Context(), false)
	if err == nil {
		t.Fatal("the relay must fail closed without a server signing key")
	}
	if !strings.Contains(err.Error(), "signing key") {
		t.Fatalf("the error must name the missing signing key, got %q", err)
	}
}

func TestDeviceIdentityLivesOutsideCredentialsJSON(t *testing.T) {
	isolate(t)
	p, err := deviceIdentityPath()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(p) == "credentials.json" {
		t.Fatal("device identity must not share a file with the session key")
	}
}
