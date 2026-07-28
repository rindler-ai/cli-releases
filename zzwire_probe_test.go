package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// The server's success_rate is ALREADY a percentage: api/action_outcome.go
// outcomeCompletionRate returns completed/(completed+failed)*100, and
// api/workspace_member_usage_floor_test.go asserts `success_rate = 20 (a
// percentage, not a fraction)`.
func TestProbeSuccessRateScale(t *testing.T) {
	body := `{"window_days":30,"end_at":"2026-07-28T00:00:00Z",
	  "mine":{"actor":"you","actions":412,"successes":400,"completed":370,
	          "handed_off":30,"failed":12,"blocked":0,"success_rate":96.9,"credits":57},
	  "workspace_totals":{"actions":9130,"successes":8402,"blocked":728,"credits":1244}}`
	var u usageResponse
	if err := json.Unmarshal([]byte(body), &u); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	printUsage(&b, u, scopeMe)
	t.Logf("me scope:\n%s", b.String())

	var w strings.Builder
	printUsage(&w, u, scopeWorkspace)
	t.Logf("workspace scope:\n%s", w.String())
}
