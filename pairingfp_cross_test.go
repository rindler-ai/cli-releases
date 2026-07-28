package main

import (
	"encoding/hex"
	"testing"
)

// Cross-check against a fingerprint computed by the SERVER's own
// devicerelay.PairingFingerprint. A mirrored crypto contract that agrees in
// shape but not in bytes is worse than none: it would refuse every real pairing.
func TestFingerprintMatchesTheServerByteForByte(t *testing.T) {
	pub, _ := hex.DecodeString("07bb78ae6bbe8c0a151295d3502ec4f1d5a84aa365e56e0d5da7216f4819b20a")
	const fromServer = "c3d9309ac00273eefc6320f2236b92ad"
	got := hex.EncodeToString(pairingFingerprint(pub))
	if got != fromServer {
		t.Fatalf("CLI computed %s, server computed %s -- the mirror is wrong and every pairing would refuse", got, fromServer)
	}
}
