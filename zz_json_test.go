package main

import (
	"encoding/json"
	"testing"
)

// A realistic terminal envelope as api.RuntimeJobEnvelope marshals it.
const serverEnvelope = `{"id":"j1","task_id":"","session_id":"sess-9","task_version":0,
"status":"succeeded","steps_completed":3,"steps_total":3,
"started_at":"2026-07-27T10:00:00Z","finished_at":"2026-07-27T10:01:00Z",
"job_id":"j1","verb":"run","site":"example.com","config_version":4,
"outputs":{"records":[{"title":"A"}],"total":12},
"result":{"anything":true},
"evidence":{"run_viewer_url":"https://app/runs/j1","screenshots":["s1"]},
"retrieval":{"outcome":"ok","complete":true},
"usage":{"outcome_count":1,"steps":3}}`

func TestZZRunJSONIsLossy(t *testing.T) {
	var env runJobEnvelope
	if err := json.Unmarshal([]byte(serverEnvelope), &env); err != nil {
		t.Fatal(err)
	}
	b, _ := json.MarshalIndent(env, "", "  ")
	var before, after map[string]any
	_ = json.Unmarshal([]byte(serverEnvelope), &before)
	_ = json.Unmarshal(b, &after)
	for k := range before {
		if _, ok := after[k]; !ok {
			t.Logf("run --json DROPS %q", k)
		}
	}
	t.Logf("%s", b)
}
