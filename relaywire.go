package main

// The device-relay wire contract, mirrored from the Rindler server.
//
// This repo is public and the server's Go package lives in a private module, so
// the contract is mirrored rather than imported. That is the intended shape --
// the server documents the contract as language-agnostic precisely so non-Go
// custody clients can implement it -- but it means the two can drift, so every
// constant below is byte-exact and covered by relaysign_test.go's golden vectors.
//
// Nothing here is a secret. These are domain-separation tags and message shapes;
// the security of the relay rests entirely on the Ed25519 and HPKE keys, never on
// the wire format being private.
//
// INVARIANT (inherited from the server): no type here may carry a durable secret.
// A ping asks for exactly ONE secret and the reply seals exactly that one -- the
// vault never crosses the wire.

// relayProtocolVersion is the wire version this client speaks. v2 added the
// server-signed ping; a v1-only server cannot be trusted to sign, so there is
// deliberately no downgrade path.
const relayProtocolVersion = 2

// Domain-separation tags. Changing any of these invalidates every signature and
// ciphertext by design, so they must match the server exactly.
const (
	pingSigningTag    = "rindler-device-relay/ping/v2"
	releaseSigningTag = "rindler-device-relay/release/v2"
	sealInfoPrefix    = "rindler-device-relay-v1"
)

// secretKind is the single secret one ping asks for. Closed set: an unknown kind
// is refused rather than guessed at, because guessing means releasing the wrong
// secret to a caller that asked for something else.
type secretKind string

const (
	secretUsername     secretKind = "username"
	secretPassword     secretKind = "password"
	secretEmailOTPCode secretKind = "email_otp_code"
	secretSMSOTPCode   secretKind = "sms_otp_code"
	secretManualCode   secretKind = "manual_code"
)

// validSecretKind reports whether k is one this client can serve from the vault.
// The OTP kinds are deliberately absent: the vault stores durable credentials,
// and a live one-time code is not something it can ever hold.
func validSecretKind(k secretKind) bool {
	switch k {
	case secretUsername, secretPassword:
		return true
	default:
		return false
	}
}

// secretPing is server -> device: "seal this one secret to this worker key".
type secretPing struct {
	RequestID             string     `json:"request_id"`
	Site                  string     `json:"site"`
	SecretKind            secretKind `json:"secret_kind"`
	WorkerEphemeralPubkey []byte     `json:"worker_ephemeral_pubkey"`
	Challenge             []byte     `json:"challenge"`
	TTLSeconds            int        `json:"ttl_seconds"`
	ServerSignature       []byte     `json:"server_signature"`
}

// secretRelease is device -> server: the sealed secret plus our signature over
// it. SealedSecret opens ONLY in the worker that minted the ephemeral key, so
// the server relays a ciphertext it cannot itself read.
type secretRelease struct {
	RequestID       string `json:"request_id"`
	SealedSecret    []byte `json:"sealed_secret"`
	DeviceSignature []byte `json:"device_signature"`
}

type relayAck struct {
	RequestID string `json:"request_id"`
	Status    string `json:"status"`
}

type siteInventoryRequest struct {
	RequestID string `json:"request_id"`
}

// siteInventoryReply tells the server which sites this device can serve, so the
// runtime can route a login here instead of dead-ending. Only domains are sent:
// never a username, never a secret.
type siteInventoryReply struct {
	RequestID string   `json:"request_id"`
	Domains   []string `json:"domains"`
	Paused    bool     `json:"paused,omitempty"`
}

// relayWire is the envelope every frame travels in, both directions.
type relayWire struct {
	Type           string                `json:"type"`
	Token          string                `json:"token,omitempty"`
	V              int                   `json:"v,omitempty"`
	DeviceID       string                `json:"device_id,omitempty"`
	Error          string                `json:"error,omitempty"`
	RequestID      string                `json:"request_id,omitempty"`
	Ping           *secretPing           `json:"ping,omitempty"`
	Release        *secretRelease        `json:"release,omitempty"`
	Ack            *relayAck             `json:"ack,omitempty"`
	Inventory      *siteInventoryRequest `json:"inventory,omitempty"`
	InventoryReply *siteInventoryReply   `json:"inventory_reply,omitempty"`
}
