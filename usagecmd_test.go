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
  "mine": {"actor":"you","actions":412,"successes":389,"blocked":23,
           "completed":370,"handed_off":30,"failed":12,"unclassified":0,
           "success_rate":96.9,"credits":57,"last_active_at":"2026-07-27T09:00:00Z"},
  "workspace_totals": {"actions":9130,"successes":8402,"blocked":728,"credits":1244},
  "unattributed": {"actor":"unattributed","actions":88,"successes":80,"blocked":8,"success_rate":90.9,"credits":11},
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
	// The OUTCOME vocabulary, not raw successes. `successes` counts calls that
	// did not error; `completed` counts calls that finished the work, and the
	// rate is derived from completed/(completed+failed) -- so printing the rate
	// beside `successes` paired two different measurements and overstated the
	// rate whenever work was handed off.
	for _, want := range []string{
		"412",  // actions
		"370",  // completed
		"97%",  // the rate, which belongs to completed AND arrives as a percentage
		"30",   // handed back — the auth-wall bucket a CLI user needs
		"12",   // failed
		"57",   // credits
		"88",   // the unattributed remainder
		"9130", // the workspace total, for context
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q\n---\n%s", want, out)
		}
	}
	if strings.Contains(out, "succeeded 389") {
		t.Error("successes must not be printed beside a rate derived from completed/failed")
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
	printDisclosures(&b, u)
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
	// A PERCENTAGE, matching the units the server uses for the per-member rate.
	// A fraction here would make the personal and workspace views of one
	// measurement disagree by a factor of 100.
	if got := rate(5, 10); got != 50 {
		t.Fatalf("rate(5,10) = %v, want 50 (a percentage, not a fraction)", got)
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

// The outcome fields must actually DECODE. Under-transcribing this struct is
// how this command shipped a fully zeroed report once already, and five of
// these fields were missing from the second version too.
func TestEveryOutcomeFieldDecodes(t *testing.T) {
	var u usageResponse
	if err := json.Unmarshal([]byte(realServerBody), &u); err != nil {
		t.Fatal(err)
	}
	for name, got := range map[string]int64{
		"completed":  u.Mine.Completed,
		"handed_off": u.Mine.HandedOff,
		"failed":     u.Mine.Failed,
	} {
		if got == 0 {
			t.Errorf("%s decoded to 0; the field is missing from the struct", name)
		}
	}
	if u.Mine.LastActiveAt == "" {
		t.Error("last_active_at decoded empty")
	}
}

// handed_off is the bucket that changes what a reader DOES: the work is not
// broken, it is waiting on them. It must be named as such, not folded into
// failures.
func TestHandedOffIsNotReportedAsFailure(t *testing.T) {
	var u usageResponse
	_ = json.Unmarshal([]byte(realServerBody), &u)
	var b strings.Builder
	printUsage(&b, u, scopeMe)
	out := b.String()
	if !strings.Contains(out, "handed back") {
		t.Errorf("the handed-off bucket is not surfaced:\n%s", out)
	}
	if !strings.Contains(out, "needed you") {
		t.Errorf("the reader is not told handed-off means it needs them:\n%s", out)
	}
}

// Zero-valued buckets stay off the page: a row of zeros reads as breakage.
func TestEmptyBucketsAreOmitted(t *testing.T) {
	clean := `{"window_days":30,"start_at":"2026-06-27T00:00:00Z","end_at":"2026-07-27T00:00:00Z",
	           "mine":{"actor":"you","actions":10,"completed":10,"handed_off":0,"failed":0,
	                   "blocked":0,"unclassified":0,"success_rate":100,"credits":3},
	           "workspace_totals":{"actions":10,"successes":10,"blocked":0,"credits":3},
	           "unattributed":{"actor":"unattributed","actions":0},
	           "credits_reconstructed":true,"visible_to_admins":true}`
	var u usageResponse
	if err := json.Unmarshal([]byte(clean), &u); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	printUsage(&b, u, scopeMe)
	out := b.String()
	for _, absent := range []string{"handed back", "failed", "blocked", "unclassified"} {
		if strings.Contains(out, absent) {
			t.Errorf("a clean run still printed %q:\n%s", absent, out)
		}
	}
	if !strings.Contains(out, "completed    10") {
		t.Errorf("the completed count is missing:\n%s", out)
	}
}

// success_rate ARRIVES AS A PERCENTAGE, and this is the third time an invented
// fixture hid a units bug in this file.
//
// The server's outcomeCompletionRate returns completed/resolved*100 rounded to
// one decimal, and its own test asserts "a percentage, not a fraction". This CLI
// multiplied it by 100 again, so a real 94.4 printed as 9440% — shipped in v0.4.0
// through v0.7.1, invisible because the fixture said 0.9442.
//
// Pinned with a realistic percentage and an assertion on the RENDERED output,
// because the render is where the units error appeared.
func TestTheSuccessRateIsTreatedAsAPercentage(t *testing.T) {
	body := `{"window_days":30,"end_at":"2026-07-28T00:00:00Z",
	          "mine":{"actor":"you","actions":412,"completed":370,"failed":12,
	                  "success_rate":96.9,"credits":57},
	          "workspace_totals":{"actions":9130,"successes":8402,"blocked":728,"credits":1244},
	          "unattributed":{"actor":"unattributed","actions":0},
	          "credits_reconstructed":true,"visible_to_admins":true}`
	var u usageResponse
	if err := json.Unmarshal([]byte(body), &u); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	printUsage(&b, u, scopeMe)
	out := b.String()

	if !strings.Contains(out, "97% success rate") {
		t.Errorf("want 97%%, got:\n%s", out)
	}
	// The specific catastrophe: multiplying a percentage by 100 again.
	for _, absurd := range []string{"9690%", "9440%", "969%"} {
		if strings.Contains(out, absurd) {
			t.Errorf("rendered %s — a percentage was multiplied by 100 again:\n%s", absurd, out)
		}
	}

	// The workspace view derives its own rate and must use the SAME units, or one
	// measurement reads differently at two scopes.
	var wb strings.Builder
	printUsage(&wb, u, scopeWorkspace)
	// 8402/9130 = 92%.
	if !strings.Contains(wb.String(), "92%") {
		t.Errorf("the workspace rate is in different units:\n%s", wb.String())
	}
}
