package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// burnRate and daysLeft restate numbers already on screen, so a reader can
// check them. That only holds if they refuse to invent anything.
func TestBurnRateRefusesWhatItCannotCompute(t *testing.T) {
	for _, tc := range []struct {
		name    string
		credits int64
		days    int
		want    float64
	}{
		{"normal", 60, 30, 2},
		{"no spend", 0, 30, 0},
		{"no window", 60, 0, 0},
		// Negative is not physically possible, but a wrong value must not become
		// a negative burn and then a negative runway.
		{"negative credits", -5, 30, 0},
		{"negative window", 60, -1, 0},
	} {
		if got := burnRate(tc.credits, tc.days); got != tc.want {
			t.Errorf("%s: burnRate(%d,%d) = %v, want %v", tc.name, tc.credits, tc.days, got, tc.want)
		}
	}
}

// A projection must never divide by a zero burn: that yields infinity and
// prints a promise.
func TestDaysLeftNeverDividesByZero(t *testing.T) {
	if got := daysLeft(1000, 0); got != 0 {
		t.Fatalf("daysLeft with no burn = %d, want 0 (no projection)", got)
	}
	if got := daysLeft(0, 5); got != 0 {
		t.Errorf("daysLeft with no balance = %d, want 0", got)
	}
	if got := daysLeft(100, 10); got != 10 {
		t.Errorf("daysLeft(100, 10) = %d, want 10", got)
	}
}

// A tiny burn over a short window extrapolates absurdly far. "Your credits last
// 9000 days" is noise, so the projection is capped.
func TestTheProjectionIsCapped(t *testing.T) {
	got := daysLeft(1_000_000, 0.01)
	if got > 999 {
		t.Fatalf("daysLeft = %d; an absurd projection must be capped", got)
	}
}

// A burn of zero must print NOTHING, not "0.0 credits/day". A window with no
// spend and a window we cannot measure look identical in the arithmetic, so
// neither may be rendered as a measured zero.
func TestAnUncomputableBurnIsOmittedNotZeroed(t *testing.T) {
	var u usageResponse
	_ = json.Unmarshal([]byte(`{"window_days":30,"end_at":"2026-07-27T00:00:00Z",
	    "mine":{"actor":"you","actions":5,"credits":0}}`), &u)
	var b strings.Builder
	printBurn(&b, u, nil, scopeMe)
	if strings.Contains(b.String(), "burn") {
		t.Fatalf("a zero burn was rendered:\n%s", b.String())
	}
}

// A projection needs a balance we KNOW. known:false means the server could not
// read it — projecting from that would be inventing a runway.
func TestNoRunwayFromAnUnknownBalance(t *testing.T) {
	var u usageResponse
	_ = json.Unmarshal([]byte(`{"window_days":30,"end_at":"2026-07-27T00:00:00Z",
	    "mine":{"actor":"you","actions":100,"credits":60}}`), &u)

	// known:false WITH numbers attached is the dangerous shape: the server is
	// saying "do not trust these". A fixture with remaining:0 would pass even
	// without the guard, because the projection would be zero anyway — so it has
	// to carry a number big enough to produce a runway if the guard were gone.
	var unknown creditsResponse
	_ = json.Unmarshal([]byte(
		`{"pool":"workspace","workspace_credit":{"known":false,"remaining":5000,"allotment":10000}}`), &unknown)
	var b strings.Builder
	printBurn(&b, u, &unknown, scopeMe)
	out := b.String()
	if !strings.Contains(out, "burn") {
		t.Error("the burn itself is computable and should print")
	}
	if strings.Contains(out, "runway") {
		t.Errorf("projected a runway from a balance the server could not read:\n%s", out)
	}

	var known creditsResponse
	_ = json.Unmarshal([]byte(`{"pool":"workspace","workspace_credit":{"known":true,"remaining":200,"allotment":1000}}`), &known)
	var b2 strings.Builder
	printBurn(&b2, u, &known, scopeMe)
	if !strings.Contains(b2.String(), "runway") {
		t.Errorf("a known balance should yield a projection:\n%s", b2.String())
	}
	// 200 remaining at 2/day = 100 days.
	if !strings.Contains(b2.String(), "100") {
		t.Errorf("wrong projection:\n%s", b2.String())
	}
}
