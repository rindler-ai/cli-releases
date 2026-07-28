package main

import (
	"crypto/ed25519"
	"encoding/hex"
	"testing"
)

// PAIRING-CHANNEL TOFU, device side.
//
// A cd_pair2_ pairing code carries a fingerprint of the lane's relay signing key.
// The device must confirm the server_pubkey it is handed at pair/complete hashes
// to that fingerprint.
//
// The server checks the same fingerprint at redeem, and its own comment says that
// check is EARLY DETECTION while the device-side one is load-bearing. The reason
// is precise: the server's check catches a lane-key rotation between mint and
// redeem, but cannot catch a substituted RESPONSE, because whoever rewrites the
// response body is not the party the server is checking.
//
// What the device-side check buys: the pairing code arrives over the
// authenticated init call, so its fingerprint is a commitment made before any
// response exists. Without the check, someone able to alter only the
// pair/complete response hands this machine a relay key they control -- and then
// signs SecretPings this device trusts, which is credential extraction.

func fpToken(t *testing.T, pub ed25519.PublicKey) string {
	t.Helper()
	nonce := make([]byte, pairingNonceLen) // zeros are fine; only the FP half is checked
	return pairingTokenPrefix2 + hex.EncodeToString(append(nonce, pairingFingerprint(pub)...))
}

func TestTheCommittedKeyIsAccepted(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !serverKeyMatchesPairingCode(fpToken(t, pub), pub) {
		t.Fatal("the key the code committed to was rejected; pairing would never succeed")
	}
}

// THE ATTACK. A different key must be refused.
func TestASubstitutedRelayKeyIsRefused(t *testing.T) {
	committed, _, _ := ed25519.GenerateKey(nil)
	attacker, _, _ := ed25519.GenerateKey(nil)

	if serverKeyMatchesPairingCode(fpToken(t, committed), attacker) {
		t.Fatal("a substituted relay key was accepted; the device would trust the attacker's SecretPings")
	}
	// A single flipped byte must also fail: the fingerprint is a hash, not a
	// prefix check.
	tampered := append([]byte(nil), committed...)
	tampered[0] ^= 0x01
	if serverKeyMatchesPairingCode(fpToken(t, committed), tampered) {
		t.Fatal("a one-byte-different key was accepted")
	}
}

// A malformed cd_pair2_ code FAILS CLOSED. A code that announces a fingerprint
// and then does not carry a readable one is not a code to trust.
func TestAMalformedFingerprintedCodeFailsClosed(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	for _, bad := range []string{
		pairingTokenPrefix2,                                        // nothing after the prefix
		pairingTokenPrefix2 + "not-hex",                            // unreadable
		pairingTokenPrefix2 + "abcd",                               // too short
		pairingTokenPrefix2 + hex.EncodeToString(make([]byte, 8)),  // short
		pairingTokenPrefix2 + hex.EncodeToString(make([]byte, 64)), // long
	} {
		if serverKeyMatchesPairingCode(bad, pub) {
			t.Errorf("malformed code %q was accepted", bad)
		}
	}
}

// A code WITHOUT the prefix carries nothing to verify, and must not be refused --
// that would break pairing against a lane that has not adopted fingerprinted
// codes. This is the one soft edge, and it matches the server's own position.
func TestAnUnfingerprintedCodeIsNotRefused(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	for _, legacy := range []string{"cd_pair_oldstyle", "pt-1", ""} {
		if !serverKeyMatchesPairingCode(legacy, pub) {
			t.Errorf("legacy code %q was refused; there is nothing in it to check", legacy)
		}
	}
}

// The fingerprint must be DOMAIN-SEPARATED, or a hash computed for another
// purpose over the same key could be replayed as a pairing commitment.
func TestTheFingerprintIsDomainSeparated(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	fp := pairingFingerprint(pub)
	if len(fp) != pairingFPLen {
		t.Fatalf("fingerprint is %d bytes, want %d", len(fp), pairingFPLen)
	}
	if pairingFPTag == "" {
		t.Fatal("the fingerprint must carry a domain-separation tag")
	}
	// A different key gives a different fingerprint (the whole point).
	other, _, _ := ed25519.GenerateKey(nil)
	if string(fp) == string(pairingFingerprint(other)) {
		t.Fatal("two different keys share a fingerprint")
	}
}
