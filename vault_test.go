package main

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVaultSealOpenRoundTrip(t *testing.T) {
	isolate(t)
	key, _, err := vaultMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	in := vaultSecret{Username: "john@example.com", Password: "correct horse battery"}
	nonce, ct, err := vaultSeal(key, "mail.google.com", in)
	if err != nil {
		t.Fatal(err)
	}
	got, err := vaultOpen(key, vaultRecord{Site: "mail.google.com", Nonce: nonce, Cipher: ct})
	if err != nil {
		t.Fatal(err)
	}
	if got != in {
		t.Errorf("round trip = %+v, want %+v", got, in)
	}
}

// The site is authenticated data, so a ciphertext lifted onto another site's
// record must not open. Without that, editing one field of the JSON file would
// let a credential be silently reassigned to a different site.
func TestVaultRecordIsBoundToItsSite(t *testing.T) {
	isolate(t)
	key, _, _ := vaultMasterKey()
	nonce, ct, err := vaultSeal(key, "mail.google.com", vaultSecret{Username: "u", Password: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := vaultOpen(key, vaultRecord{Site: "evil.example", Nonce: nonce, Cipher: ct}); err == nil {
		t.Fatal("a record moved to another site must NOT decrypt")
	}
}

func TestVaultRejectsTamperedCiphertext(t *testing.T) {
	isolate(t)
	key, _, _ := vaultMasterKey()
	nonce, ct, _ := vaultSeal(key, "a.com", vaultSecret{Username: "u", Password: "p"})
	raw, _ := base64.StdEncoding.DecodeString(ct)
	raw[0] ^= 0xff
	if _, err := vaultOpen(key, vaultRecord{
		Site: "a.com", Nonce: nonce, Cipher: base64.StdEncoding.EncodeToString(raw),
	}); err == nil {
		t.Fatal("a flipped ciphertext bit must fail the GCM tag")
	}
}

func TestVaultWrongKeyCannotOpen(t *testing.T) {
	isolate(t)
	key, _, _ := vaultMasterKey()
	nonce, ct, _ := vaultSeal(key, "a.com", vaultSecret{Username: "u", Password: "p"})
	other := make([]byte, 32)
	other[0] = 1
	if _, err := vaultOpen(other, vaultRecord{Site: "a.com", Nonce: nonce, Cipher: ct}); err == nil {
		t.Fatal("a different key must not open the record")
	}
}

// Each seal must use a fresh nonce. A repeated nonce under the same key is a
// catastrophic GCM failure, not a cosmetic one.
func TestVaultUsesAFreshNoncePerSeal(t *testing.T) {
	isolate(t)
	key, _, _ := vaultMasterKey()
	seen := map[string]bool{}
	for i := 0; i < 25; i++ {
		n, _, err := vaultSeal(key, "a.com", vaultSecret{Username: "u", Password: "p"})
		if err != nil {
			t.Fatal(err)
		}
		if seen[n] {
			t.Fatalf("nonce reused after %d seals", i)
		}
		seen[n] = true
	}
}

func TestVaultMasterKeyIsStableAcrossCalls(t *testing.T) {
	isolate(t)
	a, _, err := vaultMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := vaultMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Fatal("the master key must persist; regenerating it would orphan every stored credential")
	}
	if len(a) != 32 {
		t.Fatalf("key length = %d, want 32", len(a))
	}
}

// The whole point: the plaintext must not be recoverable from the files on disk.
func TestVaultFileNeverContainsThePlaintext(t *testing.T) {
	dir := isolate(t)
	const pw = "s3cr3t-passphrase-xyz"
	const user = "john@example.com"

	key, _, _ := vaultMasterKey()
	nonce, ct, _ := vaultSeal(key, "mail.google.com", vaultSecret{Username: user, Password: pw})
	if err := saveVault(vaultFile{Version: 1, Records: []vaultRecord{{
		Site: "mail.google.com", Nonce: nonce, Cipher: ct, CreatedAt: "now",
	}}}); err != nil {
		t.Fatal(err)
	}

	// Scan EVERY file the CLI wrote, not just the vault: a leak into a sibling
	// file counts just the same.
	var found []string
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return nil
		}
		if strings.Contains(string(b), pw) || strings.Contains(string(b), user) {
			found = append(found, p)
		}
		return nil
	})
	if len(found) > 0 {
		t.Errorf("plaintext credential found on disk in: %v", found)
	}
}

func TestVaultRoundTripThroughDispatch(t *testing.T) {
	isolate(t)

	// add (password arrives on stdin, never as a flag)
	r, w, _ := os.Pipe()
	orig := os.Stdin
	os.Stdin = r
	go func() { _, _ = w.WriteString("pw-from-stdin\n"); _ = w.Close() }()
	code := run([]string{"creds", "add", "mail.google.com", "--username", "john@example.com", "--otp", "email"})
	os.Stdin = orig
	if code != 0 {
		t.Fatalf("creds add should exit 0, got %d", code)
	}

	if code := run([]string{"creds", "list"}); code != 0 {
		t.Errorf("creds list should exit 0, got %d", code)
	}
	if code := run([]string{"creds", "show", "mail.google.com"}); code != 0 {
		t.Errorf("creds show should exit 0, got %d", code)
	}
	// Unknown site is an error, not a silent success.
	if code := run([]string{"creds", "show", "nope.example"}); code == 0 {
		t.Error("showing an unstored site should exit non-zero")
	}
	if code := run([]string{"creds", "rm", "mail.google.com"}); code != 0 {
		t.Errorf("creds rm should exit 0, got %d", code)
	}
	if code := run([]string{"creds", "rm", "mail.google.com"}); code == 0 {
		t.Error("removing a missing credential should exit non-zero")
	}
}

func TestCredsUsageErrors(t *testing.T) {
	isolate(t)
	if code := run([]string{"creds"}); code != 2 {
		t.Errorf("bare creds should be a usage error, got %d", code)
	}
	if code := run([]string{"creds", "wat"}); code != 2 {
		t.Errorf("unknown subcommand should be a usage error, got %d", code)
	}
	if code := run([]string{"creds", "add", "example.com"}); code != 2 {
		t.Errorf("add without --username should be a usage error, got %d", code)
	}
	if code := run([]string{"creds", "add", "example.com", "--username", "u", "--otp", "carrier-pigeon"}); code != 2 {
		t.Errorf("an unsupported --otp should be refused, got %d", code)
	}
}

// Re-adding a site REPLACES the record. Appending would leave the previous
// password sitting in the file, readable with the same key.
func TestVaultAddReplacesRatherThanAppends(t *testing.T) {
	isolate(t)
	key, _, _ := vaultMasterKey()
	n1, c1, _ := vaultSeal(key, "a.com", vaultSecret{Username: "u", Password: "old"})
	vf := vaultFile{Version: 1, Records: []vaultRecord{{Site: "a.com", Nonce: n1, Cipher: c1, CreatedAt: "t0"}}}
	if err := saveVault(vf); err != nil {
		t.Fatal(err)
	}

	r, w, _ := os.Pipe()
	orig := os.Stdin
	os.Stdin = r
	go func() { _, _ = w.WriteString("new\n"); _ = w.Close() }()
	code := run([]string{"creds", "add", "a.com", "--username", "u"})
	os.Stdin = orig
	if code != 0 {
		t.Fatalf("re-add should succeed, got %d", code)
	}

	got, err := loadVault()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Records) != 1 {
		t.Fatalf("re-adding must replace, got %d records", len(got.Records))
	}
	sec, err := vaultOpen(key, got.Records[0])
	if err != nil {
		t.Fatal(err)
	}
	if sec.Password != "new" {
		t.Errorf("stored password = %q, want the new one", sec.Password)
	}
	if got.Records[0].CreatedAt != "t0" || got.Records[0].UpdatedAt == "" {
		t.Errorf("replace should keep created_at and stamp updated_at, got %+v", got.Records[0])
	}
}
