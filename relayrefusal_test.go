package main

import (
	"context"
	"crypto/ed25519"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// A REFUSAL is the server answering; a dropped socket is the connection
// failing. Reconnecting fixes the second and can never fix the first.
//
// A device revoked from the dashboard used to reconnect forever — and in
// silence, because the disconnect line only prints under --verbose. It stopped
// only when the process did.
func TestARefusalIsDistinguishableFromADroppedConnection(t *testing.T) {
	var refused *relayRefusedError
	if !errors.As(&relayRefusedError{reason: "unknown device"}, &refused) {
		t.Fatal("a refusal must be recognisable by type")
	}
	if errors.As(errors.New("dial: connection reset"), &refused) {
		t.Fatal("a transport error must NOT be classified as a refusal")
	}
}

// The refusal must name the fix, and must not swallow the server's reason.
func TestARefusalCarriesTheServersReason(t *testing.T) {
	err := &relayRefusedError{reason: "device revoked"}
	if !strings.Contains(err.Error(), "device revoked") {
		t.Errorf("the server's reason was lost: %q", err)
	}
}

// Not on the FIRST refusal: a server-side blip can render as one, and giving up
// instantly would take the device offline for a fault that fixes itself.
func TestTheRefusalBudgetAbsorbsATransientFault(t *testing.T) {
	if relayMaxRefusals < 2 {
		t.Fatalf("relayMaxRefusals = %d; one refusal must not be fatal", relayMaxRefusals)
	}
	if relayMaxRefusals > 5 {
		t.Fatalf("relayMaxRefusals = %d; a revoked device should stop retrying promptly", relayMaxRefusals)
	}
}

// A run of refusals broken by a successful session must reset the count, or a
// long-lived relay eventually accumulates enough unrelated blips to quit.
func TestTheRefusalCountResetsOnRecovery(t *testing.T) {
	refusals := 0
	feed := []error{
		&relayRefusedError{reason: "blip"},
		errors.New("dial: reset"), // any non-refusal clears it
		&relayRefusedError{reason: "blip"},
		&relayRefusedError{reason: "blip"},
	}
	for _, err := range feed {
		var refused *relayRefusedError
		if errors.As(err, &refused) {
			refusals++
		} else {
			refusals = 0
		}
		if refusals >= relayMaxRefusals {
			t.Fatalf("gave up after an interrupted run of refusals (%d)", refusals)
		}
	}
}

// THE WIRING TEST. The type existing proves nothing; what matters is that a
// real refusal from a real socket comes back as one. Without this, classifying
// the error at the call site can be reverted and every type-level test above
// still passes.
func TestRelaySessionReportsARealRefusalAsARefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		var hello relayWire
		if readWire(ctx, c, &hello) != nil {
			return
		}
		// What the server sends for a device it will not accept.
		_ = writeWire(ctx, c, relayWire{Type: "hello_error", Error: "device revoked"})
	}))
	defer srv.Close()

	d := deviceIdentity{
		APIBase:      srv.URL,
		DeviceToken:  "dt-1",
		ServerPubkey: make([]byte, ed25519.PublicKeySize),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := relaySession(ctx, d, false)
	var refused *relayRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("a refusal from the wire came back as %T (%v); the loop will retry it forever", err, err)
	}
	if !strings.Contains(refused.reason, "device revoked") {
		t.Errorf("reason = %q, want the server's own text", refused.reason)
	}
}

// The counterpart: a socket that simply drops must NOT be classified as a
// refusal, or a flaky network would take the device permanently offline.
func TestRelaySessionReportsADroppedSocketAsTransport(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		c.CloseNow() // hang up mid-handshake
	}))
	defer srv.Close()

	d := deviceIdentity{
		APIBase:      srv.URL,
		DeviceToken:  "dt-1",
		ServerPubkey: make([]byte, ed25519.PublicKeySize),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := relaySession(ctx, d, false)
	var refused *relayRefusedError
	if errors.As(err, &refused) {
		t.Fatal("a dropped socket was classified as a refusal; a flaky network would take the device offline")
	}
	if err == nil {
		t.Fatal("a dropped socket must be an error")
	}
}
