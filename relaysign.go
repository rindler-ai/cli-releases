package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"strconv"
	"strings"
)

// Signing-message construction, mirrored byte-for-byte from the server.
//
// Every field is length-prefixed with a big-endian uint32 before its bytes. That
// framing is the whole point: without it, a signature over concatenated fields
// could be replayed with the boundaries moved (a request_id ending in what the
// next field started with), so the length prefix is what makes the message
// unambiguous rather than merely long.

func encField(b *bytes.Buffer, x []byte) {
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(x)))
	b.Write(n[:])
	b.Write(x)
}

// pingSigningMessage rebuilds exactly what the server signed, so we can verify
// it. It binds the worker's ephemeral pubkey into the signature: without that,
// a server that had been compromised could keep a valid old signature and swap
// in an attacker's recipient key, and the device would seal the secret straight
// to the attacker.
func pingSigningMessage(p secretPing) []byte {
	var b bytes.Buffer
	encField(&b, []byte(pingSigningTag))
	encField(&b, []byte(p.RequestID))
	encField(&b, []byte(p.Site))
	encField(&b, []byte(p.SecretKind))
	encField(&b, p.WorkerEphemeralPubkey)
	encField(&b, p.Challenge)
	encField(&b, []byte(strconv.Itoa(p.TTLSeconds)))
	return b.Bytes()
}

// releaseSigningMessage is what THIS device signs over its sealed reply, proving
// the ciphertext came from the paired device and answers this exact challenge.
func releaseSigningMessage(requestID string, challenge, workerPub, sealed []byte) []byte {
	var b bytes.Buffer
	encField(&b, []byte(releaseSigningTag))
	encField(&b, []byte(requestID))
	encField(&b, challenge)
	encField(&b, workerPub)
	encField(&b, sealed)
	return b.Bytes()
}

// verifyPingSignature checks a ping really came from the server whose public key
// we were handed at pairing.
//
// This is the single most important check in the client. Skipping it would let
// anyone who reaches the socket ask for any credential; the whole custody model
// is that the device releases a secret ONLY on a request the server signed. It
// fails closed on a missing key or signature rather than treating "unsigned" as
// "fine" -- an unsigned ping is exactly what an attacker would send.
func verifyPingSignature(serverPub ed25519.PublicKey, p secretPing) bool {
	if len(serverPub) != ed25519.PublicKeySize || len(p.ServerSignature) != ed25519.SignatureSize {
		return false
	}
	return ed25519.Verify(serverPub, pingSigningMessage(p), p.ServerSignature)
}

// Pairing-channel TOFU.
//
// A cd_pair2_ pairing code carries a 16-byte fingerprint of the LANE'S server
// signing key in its trailing bytes. The device must confirm that the
// server_pubkey it is handed at pair/complete hashes to that fingerprint.
//
// THIS IS THE LOAD-BEARING HALF, and it was missing. The server checks the same
// fingerprint at redeem and its own comment says so: "The DEVICE-side
// fingerprint check remains load-bearing; this is early detection." Early
// detection catches a lane-key rotation between mint and redeem. It cannot catch
// a substituted RESPONSE, because a party who rewrites the response body is not
// the party the server is checking.
//
// The attack the device-side check closes: the pairing code arrives over the
// authenticated init call, so its fingerprint is a commitment made before any
// response exists. Somebody able to alter only the pair/complete response would
// otherwise hand this machine a server_pubkey they control -- and from then on
// they could sign SecretPings this device trusts, which is credential
// extraction, not just a bad pairing.
const (
	pairingFPTag = "rindler-device-relay/pairing-fp/v2"
	// pairingFPLen mirrors devicerelay.PairingFingerprintLen.
	pairingFPLen = 16
	// pairingTokenPrefix2 marks a fingerprinted code. A code without it predates
	// the fingerprint and carries nothing to check.
	pairingTokenPrefix2 = "cd_pair2_"
	// pairingNonceLen is the random half that precedes the fingerprint.
	pairingNonceLen = 16
)

// pairingFingerprint recomputes the expected fingerprint for a server pubkey.
func pairingFingerprint(serverPub []byte) []byte {
	var b bytes.Buffer
	encField(&b, []byte(pairingFPTag))
	encField(&b, serverPub)
	sum := sha256.Sum256(b.Bytes())
	return sum[:pairingFPLen]
}

// serverKeyMatchesPairingCode reports whether serverPub is the key this pairing
// code committed to.
//
// FAILS CLOSED on a malformed cd_pair2_ code: a code that announces a
// fingerprint and then does not carry a readable one is not a code to trust.
//
// Returns true for a code with no prefix, because there is genuinely nothing to
// verify there and refusing would break pairing against a lane that has not
// adopted fingerprinted codes. That is the one soft edge in this check, and it is
// the server's own position: it treats such codes the same way.
func serverKeyMatchesPairingCode(pairingToken string, serverPub []byte) bool {
	body, ok := strings.CutPrefix(pairingToken, pairingTokenPrefix2)
	if !ok {
		return true
	}
	raw, err := hex.DecodeString(body)
	if err != nil || len(raw) != pairingNonceLen+pairingFPLen {
		return false
	}
	// Constant time: the comparison is against a value an attacker chooses, and
	// a byte-at-a-time answer would let them find the right one.
	return subtle.ConstantTimeCompare(raw[pairingNonceLen:], pairingFingerprint(serverPub)) == 1
}
