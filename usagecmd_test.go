package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// realServerBody is transcribed from the server's memberUsageSelfResponse, NOT
// invented here. That distinction is the whole point of this file: the previous
// version of these tests asserted an envelope the CLI had made up, so they
// passed against a fixture that shared exactly ONE field name with production.
// A fixture is only evidence if it came from the thing it stands in for.
const realServerBody = `{
  "window_days": 30,
  "start_at": "2026-06-27T00:00:00Z",
  "end_at": "2026-07-27T00:00:00Z",
  "mine": {"actor":"you","actions":412,"successes":389,"blocked":23,"success_rate":0.9442,"credits":57},
  "workspace_totals": {"actions":9130,"successes":8402,"blocked":728,"credits":1244},
  "unattributed": {"actor":"unattributed","actions":88,"successes":80,"blocked":8,"success_rate":0.909,"credits":11},
  "credits_reconstructed": true,
  "visible_to_admins": true
}`

func usageServer(t *testing.T, body string, status int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/workspace/usage/me", func(w http.ResponseWriter, r *http.Request) {
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		// `days` is the only parameter this endpoint reads; assert we never
		// invent one it will silently ignore.
		for k := range r.URL.Query() {
			if k != "days" {
				t.Errorf("sent query param %q the server does not read", k)
			}
		}
		_, _ = w.Write([]byte(body))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// THE REGRESSION TEST. The shipped CLI decoded a body like the one above into
// an all-zero struct and printed "0 used" with exit 0. Zeros decode without
// error, so only asserting on the VALUES catches it.
func TestUsageDecodesTheRealServerEnvelope(t *testing.T) {
	var u usageResponse
	if err := json.Unmarshal([]byte(realServerBody), &u); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !envelopeLooksReal(u) {
		t.Fatal("a real body must pass envelopeLooksReal")
	}
	if u.Mine.Actions != 412 || u.Mine.Successes != 389 || u.Mine.Blocked != 23 || u.Mine.Credits != 57 {
		t.Fatalf("mine decoded wrong: %+v", u.Mine)
	}
	if u.WorkspaceTotals.Actions != 9130 || u.WorkspaceTotals.Credits != 1244 {
		t.Fatalf("workspace_totals decoded wrong: %+v", u.WorkspaceTotals)
	}
	if u.Unattributed.Actions != 88 {
		t.Fatalf("unattributed decoded wrong: %+v", u.Unattributed)
	}
	if u.WindowDays != 30 || u.EndAt == "" {
		t.Fatalf("window decoded wrong: days=%d end=%q", u.WindowDays, u.EndAt)
	}
	if !u.CreditsReconstructed || !u.VisibleToAdmins {
		t.Fatal("disclosure flags decoded wrong")
	}
}

// The numbers must reach the page, not just the struct. Asserting on rendered
// output is what would have caught the original bug end to end.
func TestUsagePrintsYourRealNumbers(t *testing.T) {
	var u usageResponse
	if err := json.Unmarshal([]byte(realServerBody), &u); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	printUsage(&b, u, scopeMe)
	out := b.String()
	for _, want := range []string{"412", "389", "94%", "23", "57", "88", "9130"} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q\n---\n%s", want, out)
		}
	}
	if strings.Contains(out, "Workspace usage") {
		t.Error("the personal view must not be headed as the workspace view")
	}
}

// --workspace is a DISPLAY choice over a response that always carries both
// figures; it must show the workspace numbers, not the personal ones.
func TestUsageWorkspaceViewShowsWorkspaceNumbers(t *testing.T) {
	var u usageResponse
	if err := json.Unmarshal([]byte(realServerBody), &u); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	printUsage(&b, u, scopeWorkspace)
	out := b.String()
	if !strings.Contains(out, "Workspace usage") || !strings.Contains(out, "9130") {
		t.Errorf("workspace view wrong:\n%s", out)
	}
	if strings.Contains(out, "412") {
		t.Error("the workspace view must not print the personal action count")
	}
	// 8402/9130 = 92%; derived here because the server sends no rate for the
	// workspace row, and a silent 0% would read as a broken workspace.
	if !strings.Contains(out, "92%") {
		t.Errorf("workspace success rate not derived:\n%s", out)
	}
}

// A WRONG envelope decodes cleanly to zeros. Printing that is worse than
// printing nothing, because it looks like an answer.
func TestUsageRefusesAnEnvelopeItDoesNotRecognise(t *testing.T) {
	isolate(t)
	// The exact shape the first cut of this command invented.
	stale := `{"scope":"me","credits":{"remaining":975,"used":25,"allotment":1000},
	           "sessions":{"total":30,"refunded":5},"sites":[]}`
	srv := usageServer(t, stale, http.StatusOK)
	t.Setenv("RINDLER_API_KEY", "rindler_live_test")
	if code := run([]string{"usage", "--api-base", srv.URL}); code == 0 {
		t.Fatal("an unrecognised envelope must not exit 0 with a zeroed report")
	}
}

// A genuine zero is NOT a decode failure: a member who ran nothing this window
// is entitled to see their real zero rather than an error.
func TestUsageRendersAGenuineZero(t *testing.T) {
	isolate(t)
	empty := `{"window_days":30,"start_at":"2026-06-27T00:00:00Z","end_at":"2026-07-27T00:00:00Z",
	           "mine":{"actor":"you","actions":0,"successes":0,"blocked":0,"success_rate":0,"credits":0},
	           "workspace_totals":{"actions":0,"successes":0,"blocked":0,"credits":0},
	           "unattributed":{"actor":"unattributed","actions":0},
	           "credits_reconstructed":true,"visible_to_admins":true}`
	srv := usageServer(t, empty, http.StatusOK)
	t.Setenv("RINDLER_API_KEY", "rindler_live_test")
	if code := run([]string{"usage", "--api-base", srv.URL}); code != 0 {
		t.Fatalf("a real zero must render, got exit %d", code)
	}
}

func TestUsageEndToEndThroughDispatch(t *testing.T) {
	isolate(t)
	srv := usageServer(t, realServerBody, http.StatusOK)
	t.Setenv("RINDLER_API_KEY", "rindler_live_test")
	for _, args := range [][]string{
		{"usage", "--api-base", srv.URL},
		{"usage", "--json", "--api-base", srv.URL},
		{"usage", "--workspace", "--api-base", srv.URL},
		{"usage", "--days", "7", "--api-base", srv.URL},
	} {
		if code := run(args); code != 0 {
			t.Errorf("%v should exit 0, got %d", args, code)
		}
	}
}

func TestUsageSurfacesAnAuthFailure(t *testing.T) {
	isolate(t)
	srv := usageServer(t, "", http.StatusUnauthorized)
	t.Setenv("RINDLER_API_KEY", "rindler_live_test")
	if code := run([]string{"usage", "--api-base", srv.URL}); code == 0 {
		t.Fatal("a 401 must not exit 0")
	}
}

// Both disclosures are true of the DATA, not the surface, so the CLI owes the
// reader the same sentences the dashboard shows.
func TestUsagePrintsBothDisclosures(t *testing.T) {
	var u usageResponse
	if err := json.Unmarshal([]byte(realServerBody), &u); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	printUsage(&b, u, scopeMe)
	out := b.String()
	if !strings.Contains(out, creditsReconstructedNote) {
		t.Error("must disclose that per-member credits are reconstructed")
	}
	if !strings.Contains(out, alsoVisibleNote) {
		t.Error("must disclose that owners and admins can see the same numbers")
	}
}

func TestRateIsSafeAtZero(t *testing.T) {
	if got := rate(0, 0); got != 0 {
		t.Fatalf("rate(0,0) = %v, want 0 (no divide by zero)", got)
	}
	if got := rate(5, 10); got != 0.5 {
		t.Fatalf("rate(5,10) = %v, want 0.5", got)
	}
}

func TestDayOfTrimsOnlyRealTimestamps(t *testing.T) {
	if got := dayOf("2026-07-27T00:00:00Z"); got != "2026-07-27" {
		t.Errorf("dayOf(rfc3339) = %q", got)
	}
	// Not a timestamp: pass through rather than blindly truncating.
	for _, s := range []string{"", "unknown", "soon"} {
		if got := dayOf(s); got != s {
			t.Errorf("dayOf(%q) = %q, want passthrough", s, got)
		}
	}
}
