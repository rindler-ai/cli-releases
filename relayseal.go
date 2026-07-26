package main

import (
	"bytes"
	"crypto/ecdh"
	"crypto/hpke"
	"errors"
	"fmt"
)

// The HPKE inner seal (RFC 9180 base mode), mirrored from the server:
// DHKEM(X25519, HKDF-SHA256) + HKDF-SHA256 + AES-256-GCM.
//
// This is what makes the relay end-to-end rather than merely encrypted in
// transit. The secret is sealed to the login worker's per-login recipient key,
// so the Rindler server relays a ciphertext it cannot open. TLS alone would put
// the plaintext in the server's memory; this does not.
//
// Uses the standard library's crypto/hpke, which is also what the server uses --
// so this mirror shares an implementation rather than reimplementing the
// primitive, and the ciphersuite cannot drift by dependency skew.

func suiteKEM() hpke.KEM   { return hpke.DHKEM(ecdh.X25519()) }
func suiteKDF() hpke.KDF   { return hpke.HKDFSHA256() }
func suiteAEAD() hpke.AEAD { return hpke.AES256GCM() }

var (
	errEmptyWorkerPub = errors.New("relay: empty worker ephemeral pubkey")
	errEmptySecret    = errors.New("relay: refusing to seal an empty secret")
)

// sealInfo binds a ciphertext to exactly one request, so a secret sealed for one
// ping cannot be replayed as the answer to another. 0x1f is the unit separator
// the server uses; it must match or nothing opens.
func sealInfo(requestID, site string, kind secretKind) []byte {
	sep := []byte{0x1f}
	var b bytes.Buffer
	b.WriteString(sealInfoPrefix)
	b.Write(sep)
	b.WriteString(requestID)
	b.Write(sep)
	b.WriteString(site)
	b.Write(sep)
	b.WriteString(string(kind))
	return b.Bytes()
}

// sealToWorker encrypts one secret to the worker's ephemeral public key.
//
// Refusing an empty secret is deliberate rather than defensive noise: an empty
// seal is a well-formed ciphertext that decrypts to nothing, so the login would
// fail somewhere far away with no hint that the vault lookup was what came back
// blank.
func sealToWorker(workerPubkey, info, secret []byte) ([]byte, error) {
	if len(workerPubkey) == 0 {
		return nil, errEmptyWorkerPub
	}
	if len(secret) == 0 {
		return nil, errEmptySecret
	}
	pub, err := suiteKEM().NewPublicKey(workerPubkey)
	if err != nil {
		return nil, fmt.Errorf("relay: parse worker public key: %w", err)
	}
	ct, err := hpke.Seal(pub, suiteKDF(), suiteAEAD(), info, secret)
	if err != nil {
		return nil, fmt.Errorf("relay: hpke seal: %w", err)
	}
	return ct, nil
}
