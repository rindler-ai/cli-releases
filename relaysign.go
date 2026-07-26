package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/binary"
	"strconv"
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
