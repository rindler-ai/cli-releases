package main

import (
	"encoding/json"
	"strings"
	"testing"
)

const shapeBody = `{
  "window_days":30,"start_at":"2026-06-28T00:00:00Z","end_at":"2026-07-28T00:00:00Z",
  "mine":{"actor":"you","actions":412,"completed":370,"handed_off":30,"failed":12,
          "blocked":23,"success_rate":0.9686,"credits":57},
  "workspace_totals":{"actions":9130,"successes":8402,"blocked":728,"credits":1244},
  "unattributed":{"actor":"unattributed","actions":88},
  "sessions":{"sessions":41,"median_ms":8400,"p90_ms":94000},
  "top_actions":[
    {"action":"search_products","calls":180,"succeeded":174,"failed":6},
    {"action":"add_to_cart","calls":60,"succeeded":60,"failed":0}],
  "failure_shapes":[{"shape":"bot_wall","calls":9},{"shape":"auth_required","calls":3}],
  "credits_reconstructed":true,"visible_to_admins":true
}`

func TestTheSelfShapeSectionsRender(t *testing.T) {
	var u usageResponse
	if err := json.Unmarshal([]byte(shapeBody), &u); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	printSelfShape(&b, u)
	out := b.String()
	for _, want := range []string{
		"41",              // session count
		"8.4s",            // median, humanised
		"1m34s",           // p90 past a minute
		"search_products", // busiest action
		"6 failed",        // that action's failures
		"bot_wall",        // the failure SHAPE, the actionable part
		"9",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n---\n%s", want, out)
		}
	}
	// An action with no failures must not carry a "(0 failed)" tail.
	if strings.Contains(out, "(0 failed)") {
		t.Errorf("a clean action was annotated with zero failures:\n%s", out)
	}
}

// Absent means "the server could not read it", which must never be drawn as a
// zero. These are separately-failing extra reads server-side.
func TestAbsentSectionsAreOmittedNotZeroed(t *testing.T) {
	var u usageResponse
	if err := json.Unmarshal([]byte(realServerBody), &u); err != nil {
		t.Fatal(err)
	}
	if u.Sessions != nil {
		t.Fatal("precondition: the base fixture carries no sessions block")
	}
	var b strings.Builder
	printSelfShape(&b, u)
	if got := strings.TrimSpace(b.String()); got != "" {
		t.Fatalf("absent sections rendered something:\n%s", got)
	}
}

// A real zero is different: the server DID read it, and a member who ran nothing
// is entitled to see that. It must not be printed as a session line, though —
// "0 sessions (median 0s)" is noise.
func TestARealZeroSessionCountIsQuiet(t *testing.T) {
	body := `{"window_days":30,"end_at":"2026-07-28T00:00:00Z",
	          "mine":{"actor":"you","actions":0},
	          "sessions":{"sessions":0,"median_ms":0,"p90_ms":0}}`
	var u usageResponse
	if err := json.Unmarshal([]byte(body), &u); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	printSelfShape(&b, u)
	if strings.Contains(b.String(), "median") {
		t.Errorf("a zero-session window printed a timing line:\n%s", b.String())
	}
}

func TestHumanMsReadsTheWayPeopleThink(t *testing.T) {
	for _, tc := range []struct {
		ms   int64
		want string
	}{
		{0, "0s"}, {-5, "0s"}, {450, "450ms"},
		{8400, "8.4s"}, {59_900, "59.9s"},
		{60_000, "1m0s"}, {94_000, "1m34s"},
	} {
		if got := humanMs(tc.ms); got != tc.want {
			t.Errorf("humanMs(%d) = %q, want %q", tc.ms, got, tc.want)
		}
	}
}
