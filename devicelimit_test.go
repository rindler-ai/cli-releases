package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// "status" is the REGISTRATION state: active means enrolled and not revoked. It
// says nothing about whether the device can answer right now, so a sleeping
// laptop with no relay running still read "active" — to someone debugging why a
// login found no credential, a healthy row for the device that was not there.
func TestDeviceStatusDistinguishesRegisteredFromReachable(t *testing.T) {
	yes, no := true, false
	for _, tc := range []struct {
		name      string
		status    string
		connected *bool
		want      string
	}{
		{"live relay", "active", &yes, "active, connected"},
		{"enrolled but unreachable", "active", &no, "active, not connected"},
		// nil is UNKNOWN, not offline: the server omits the field when the hub
		// probe is unwired, and a missing field must not report every device dead.
		{"lane cannot report", "active", nil, "active"},
		{"revoked outranks reachability", "revoked", &yes, "revoked"},
	} {
		if got := deviceStatusLabel(tc.status, tc.connected); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A missing `connected` must decode to nil, not false. This is the same
// nullable-field trap as everywhere else in this repo: bool would silently
// become false and report a healthy fleet as entirely offline.
func TestAbsentConnectedDecodesToUnknownNotOffline(t *testing.T) {
	var v struct {
		Connected *bool `json:"connected,omitempty"`
	}
	if err := json.Unmarshal([]byte(`{"id":"d1","status":"active"}`), &v); err != nil {
		t.Fatal(err)
	}
	if v.Connected != nil {
		t.Fatalf("an absent connected decoded to %v, want nil (unknown)", *v.Connected)
	}
	if got := deviceStatusLabel("active", v.Connected); got != "active" {
		t.Errorf("unknown reachability rendered as %q", got)
	}
}

// --limit is the caller's ceiling on a list action. The server refuses a
// negative or oversized value rather than clamping, and treats 0/absent as "use
// the site's cap" — so sending 0 explicitly would be a request for no records.
func TestLimitIsSentOnlyWhenAsked(t *testing.T) {
	for _, tc := range []struct {
		limit     int
		wantSent  bool
		wantValue float64
	}{
		{0, false, 0},
		{25, true, 25},
	} {
		var got map[string]any
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewDecoder(r.Body).Decode(&got)
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"job_id":"j1"}`))
		}))
		_, err := startRun(context.Background(), srv.Client(), srv.URL, "k", "example.com",
			[]string{"search"}, nil, "", tc.limit)
		srv.Close()
		if err != nil {
			t.Fatalf("limit %d: %v", tc.limit, err)
		}
		v, present := got["limit"]
		if present != tc.wantSent {
			t.Errorf("limit %d: sent=%v, want %v (0 must mean 'use the site default', not 'no records')",
				tc.limit, present, tc.wantSent)
		}
		if tc.wantSent && v != tc.wantValue {
			t.Errorf("limit %d: sent %v", tc.limit, v)
		}
	}
}
