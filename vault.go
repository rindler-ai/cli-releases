// On-device encrypted credential storage.
//
// This is the CLI acting as its own Auto-Login app: site credentials are sealed
// on THIS machine and never sent to a Rindler server. The design follows the
// custody app's posture (rindler-ai/auto-login THREAT-MODEL) — durable secrets
// stay on the device, and only a single secret ever leaves it, per login, at the
// moment a login actually needs it.
//
// At rest: AES-256-GCM per record, under a 32-byte master key held in the OS
// keyring when one exists and a 0600 file otherwise — the same degradation the
// API key already accepts, reported rather than hidden.
//
// Each record gets a FRESH nonce, stored beside its ciphertext. The site is
// authenticated as additional data, so a ciphertext cannot be moved from one
// site to another inside the same file and still open.
//
// What is deliberately NOT here: printing a stored password. `creds` shows
// metadata only. A vault that can be read back on demand is a vault that a
// shoulder-surfer, a scrollback buffer, or an agent transcript can read too.

package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	vaultFileName     = "credentials.vault.json"
	vaultKeyringEntry = "rindler-vault-key"
	vaultKeyFileName  = "vault.key"
)

// vaultRecord is one site credential. Username is stored SEALED alongside the
// password: it is a personal identifier, and a vault that leaks "which accounts
// exist on which sites" is still a leak.
type vaultRecord struct {
	Site      string `json:"site"`
	Label     string `json:"label,omitempty"`
	Nonce     string `json:"nonce"`
	Cipher    string `json:"cipher"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at,omitempty"`
	// HasOTP records that this login also needs a one-time code, so `creds list`
	// can say whether a hands-off login is even possible.
	OTPMethod string `json:"otp_method,omitempty"`
}

type vaultFile struct {
	Version int           `json:"version"`
	Records []vaultRecord `json:"records"`
}

// vaultSecret is the plaintext shape sealed into each record.
type vaultSecret struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func vaultPath() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, vaultFileName), nil
}

// decodeVaultKey accepts a base64 master key only if it is exactly 32 bytes.
func decodeVaultKey(raw string) ([]byte, bool) {
	k, err := base64.StdEncoding.DecodeString(strings.TrimSpace(raw))
	if err != nil || len(k) != 32 {
		return nil, false
	}
	return k, true
}

// errVaultKeyUnavailable is returned when a vault already holds records but its
// master key cannot be found (or the keyring refused to answer). Minting a fresh
// key here would silently render every stored credential permanently unreadable,
// so we fail loudly instead and let the user fix the real problem (unlock the
// keyring, restore the key file) with the data still intact.
var errVaultKeyUnavailable = errors.New(
	"the vault master key could not be read, and this vault already holds credentials.\n" +
		"Refusing to create a new key: that would make the stored credentials unreadable.\n" +
		"If your OS keyring is locked, unlock it and retry. To start over, delete\n" +
		"credentials.vault.json and re-add the credentials.")

// vaultKeyLocations returns every place a master key can legitimately live, in
// preference order. BOTH backends are probed on every call: whether the keyring
// backend is selected depends on an external binary being on PATH
// (newSystemBackend), so the key a previous run wrote is not necessarily in the
// backend this run prefers. Probing one and minting on a miss is what destroys a
// vault, so the lookup is exhaustive and the mint is guarded.
func vaultKeyLocations(store credentialStore, dir string) []func() (string, error) {
	var probes []func() (string, error)
	if store != nil {
		probes = append(probes, func() (string, error) { return store.getNamed(vaultKeyringEntry) })
	}
	// The file store's named entry, even when a keyring is currently preferred.
	fileStore := &fileCredStore{path: filepath.Join(dir, "credentials.json")}
	probes = append(probes, func() (string, error) { return fileStore.getNamed(vaultKeyringEntry) })
	// The standalone key file.
	probes = append(probes, func() (string, error) {
		b, err := os.ReadFile(filepath.Join(dir, vaultKeyFileName))
		if err != nil {
			if os.IsNotExist(err) {
				return "", nil
			}
			return "", err
		}
		return string(b), nil
	})
	return probes
}

// vaultMasterKey returns the 32-byte key, creating it on first use. The keyring
// is preferred; the file fallback is reported to the caller so the CLI can say
// so out loud rather than silently downgrading.
//
// It NEVER mints a new key over an existing vault. A miss is only treated as
// "first use" when the vault holds no records AND no probe errored: a keyring
// that is merely locked returns an error, not absence, and treating that as
// absence would destroy the vault it was protecting.
func vaultMasterKey() (key []byte, warning string, err error) {
	store, storeWarn, err := newCredentialStore()
	if err != nil {
		store = nil
	}
	dir, err := configDir()
	if err != nil {
		return nil, "", err
	}

	probeFailed := false
	for _, probe := range vaultKeyLocations(store, dir) {
		raw, perr := probe()
		if perr != nil {
			// A backend that ERRORED has not told us the key is absent. Remember
			// that, so a locked keyring can never be read as "no key yet".
			probeFailed = true
			continue
		}
		if raw == "" {
			continue
		}
		if k, ok := decodeVaultKey(raw); ok {
			return k, storeWarn, nil
		}
	}

	// No key found. Only mint when this is genuinely first use.
	hasRecords, rerr := vaultHasRecords()
	if rerr != nil {
		return nil, "", rerr
	}
	if hasRecords || probeFailed {
		return nil, "", errVaultKeyUnavailable
	}

	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		return nil, "", fmt.Errorf("generate vault key: %w", err)
	}
	enc := base64.StdEncoding.EncodeToString(k)
	if store != nil {
		if serr := store.setNamed(vaultKeyringEntry, enc); serr == nil {
			return k, storeWarn, nil
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, "", err
	}
	if err := os.WriteFile(filepath.Join(dir, vaultKeyFileName), []byte(enc), 0o600); err != nil {
		return nil, "", fmt.Errorf("write vault key: %w", err)
	}
	return k, "vault key is stored in a 0600 file (no OS keyring available)", nil
}

// vaultHasRecords reports whether the vault file holds at least one credential.
// A missing/unreadable vault file means "no records", so first use still works.
func vaultHasRecords() (bool, error) {
	vf, err := loadVault()
	if err != nil {
		return false, nil
	}
	return len(vf.Records) > 0, nil
}

func vaultSeal(key []byte, site string, s vaultSecret) (nonceB64, cipherB64 string, err error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", "", err
	}
	plain, err := json.Marshal(s)
	if err != nil {
		return "", "", err
	}
	// site as additional data: a record cannot be relabelled to another site.
	ct := gcm.Seal(nil, nonce, plain, []byte(site))
	return base64.StdEncoding.EncodeToString(nonce), base64.StdEncoding.EncodeToString(ct), nil
}

func vaultOpen(key []byte, rec vaultRecord) (vaultSecret, error) {
	var out vaultSecret
	block, err := aes.NewCipher(key)
	if err != nil {
		return out, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return out, err
	}
	nonce, err := base64.StdEncoding.DecodeString(rec.Nonce)
	if err != nil {
		return out, err
	}
	ct, err := base64.StdEncoding.DecodeString(rec.Cipher)
	if err != nil {
		return out, err
	}
	plain, err := gcm.Open(nil, nonce, ct, []byte(rec.Site))
	if err != nil {
		return out, errors.New("could not decrypt (wrong key, or the file was tampered with)")
	}
	return out, json.Unmarshal(plain, &out)
}

func loadVault() (vaultFile, error) {
	var vf vaultFile
	p, err := vaultPath()
	if err != nil {
		return vf, err
	}
	b, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return vaultFile{Version: 1}, nil
	}
	if err != nil {
		return vf, err
	}
	if err := json.Unmarshal(b, &vf); err != nil {
		return vf, fmt.Errorf("vault file is unreadable: %w", err)
	}
	if vf.Version == 0 {
		vf.Version = 1
	}
	return vf, nil
}

func saveVault(vf vaultFile) error {
	p, err := vaultPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(vf, "", "  ")
	if err != nil {
		return err
	}
	// Write-then-rename so an interrupted save cannot truncate an existing vault.
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// normalizeVaultSite is the vault's canonical form for a site key, and it must
// stay IDENTICAL to the server's devicehub.normalizeDomain: lowercase, no
// leading "www.".
//
// Not cosmetic. The server does not ping with the domain you typed. When your
// device's advertised inventory covers the site, it pings with its OWN
// normalized form of it, so a record stored as "www.example.com" was asked for
// as "example.com" and an exact match found nothing. The relay then declined
// "no credential stored", the login carried on without one, and nothing
// anywhere reported an error -- the credential was simply unreachable for the
// rest of its life.
func normalizeVaultSite(raw string) (string, error) {
	host, err := siteFromTarget(raw)
	if err != nil {
		return "", err
	}
	return canonicalVaultSite(host), nil
}

// canonicalVaultSite mirrors the server's normalizeDomain. Kept as its own
// function so the lookup path can canonicalise a record written by an older
// build, which stored the host verbatim.
func canonicalVaultSite(host string) string {
	return strings.TrimPrefix(strings.ToLower(strings.TrimSpace(host)), "www.")
}

// findVaultRecord resolves a ping to a stored record.
//
// It compares CANONICAL forms on both sides rather than the strings as written.
// New records are already canonical, but a vault written by an earlier build
// still holds hosts verbatim, and rewriting someone's credential store on
// upgrade is a far worse trade than normalising at lookup: a bad migration
// loses secrets that cannot be recovered, while this costs one string op.
func findVaultRecord(vf vaultFile, site string) int {
	want := canonicalVaultSite(site)
	for i, r := range vf.Records {
		if canonicalVaultSite(r.Site) == want {
			return i
		}
	}
	return -1
}
