package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The server reports the credit POOL verdict and the BALANCE separately, on
// purpose, so a failed balance read cannot flip who gets debited. That means
// "we could not read your balance" and "your balance is zero" arrive as
// different things, and printing a confident 0 for the first is the same class
// of lie as the zeroed report this command shipped with.
func TestAnUnknownBalanceIsNotReportedAsZero(t *testing.T) {
	var c creditsResponse
	if err := json.Unmarshal([]byte(
		`{"pool":"workspace","workspace_credit":{"known":false,"remaining":0,"allotment":0,"used":0}}`), &c); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	printCredits(&b, &c)
	out := b.String()
	if strings.Contains(out, "0 of 0") || strings.Contains(out, "0 used") {
		t.Fatalf("an unreadable balance rendered as a real zero:\n%s", out)
	}
	if !strings.Contains(out, "unavailable") {
		t.Errorf("the reader is not told the balance is unknown:\n%s", out)
	}
}

func TestAKnownBalanceRendersTheNumbers(t *testing.T) {
	var c creditsResponse
	_ = json.Unmarshal([]byte(
		`{"pool":"workspace","workspace_credit":{"known":true,"remaining":975,"allotment":1000,"used":25}}`), &c)
	var b strings.Builder
	printCredits(&b, &c)
	out := b.String()
	for _, want := range []string{"975", "1000", "#"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// A personal-pool verdict carries no workspace balance at all. Saying which
// pool beats implying a number went missing.
func TestAPersonalPoolSaysSoRatherThanLookingBroken(t *testing.T) {
	var c creditsResponse
	_ = json.Unmarshal([]byte(`{"pool":"personal"}`), &c)
	var b strings.Builder
	printCredits(&b, &c)
	if !strings.Contains(b.String(), "personal") {
		t.Errorf("a personal pool was not named:\n%s", b.String())
	}
}

// Credits are a SECOND read against a DIFFERENT endpoint. It failing must not
// take the usage numbers down with it.
func TestACreditsFailureDoesNotHideYourUsage(t *testing.T) {
	if got := fetchCredits(context.Background(), "https://nonexistent.invalid", "k"); got != nil {
		t.Fatal("an unreachable host must yield no credits, not a fabricated zero")
	}
	var b strings.Builder
	printCredits(&b, nil)
	if !strings.Contains(b.String(), "could not read") {
		t.Errorf("a failed read must say so:\n%s", b.String())
	}
}

func TestFetchCreditsRefusesANon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"workspace_credit":{"known":true,"remaining":999999}}`))
	}))
	defer srv.Close()
	if got := fetchCredits(context.Background(), srv.URL, "k"); got != nil {
		t.Fatal("a 403 body must not be read as a balance")
	}
}

func TestCreditBarIsClamped(t *testing.T) {
	for _, tc := range []struct{ remaining, total int64 }{{-5, 100}, {150, 100}, {50, 0}, {0, 0}} {
		got := creditBar(tc.remaining, tc.total)
		if len(got) > 24 || strings.Contains(got, "-1") {
			t.Fatalf("creditBar(%d,%d) = %q", tc.remaining, tc.total, got)
		}
	}
}
