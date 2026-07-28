package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// A REST-path list run is clipped by semantic.DefaultListCap (5) unless the site
// config sets list_cap, so the server stamps truncated:true + total:N and
// DeriveRetrievalOutcome returns outcome "partial", complete:false.
func TestProbeTruncatedListRunExitCode(t *testing.T) {
	body := `{"job_id":"job-1","id":"job-1","status":"succeeded","verb":"run",
	  "site":"instacart.com",
	  "outputs":{"records":[{"n":1},{"n":2},{"n":3},{"n":4},{"n":5}],"truncated":true,"total":47},
	  "retrieval":{"outcome":"partial","complete":false,"reasons":["records_truncated"],
	               "retry_guidance":"retry_after_backoff","failure_shape":"pagination_truncated"},
	  "usage":{"outcome_count":1}}`
	var env runJobEnvelope
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	printRunResult(&b, env)
	t.Logf("printed:\n%s", b.String())
	t.Logf("runExitCode = %d", runExitCode(env))
}
