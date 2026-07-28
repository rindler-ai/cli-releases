package main

import (
	"encoding/json"
	"testing"
)

// The map status endpoint reports on TWO different things and the CLI has to
// keep them apart:
//
//	status            did the generation finish
//	acceptance_state  is the result any good
//
// durableMappingHTTPStatus reports status "complete" for a REJECTED, BLOCKED,
// NOT_PROVEN or SUPERSEDED verdict. Reading status alone therefore announced
// "✓ Mapped" and exited 0 for a mapping that published nothing — and the
// user's next run against that site failed with "unsupported site".
//
// These bodies are transcribed from api/map.go's response builder, not
// invented. The previous fixture WAS invented, which is exactly why the tests
// passed while the command was wrong.

func decodeStatus(t *testing.T, body string) mapStatusResponse {
	t.Helper()
	var st mapStatusResponse
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return st
}

func TestARejectedMappingIsNotASuccess(t *testing.T) {
	// What the server sends when the verifier rejects: status IS "complete".
	body := `{"status":"complete","acceptance_state":"rejected",
	          "message":"could not prove checkout capability","domain":"example.com"}`
	st := decodeStatus(t, body)

	if done, ok := mapTerminal(st.Status); !done || !ok {
		t.Fatalf("precondition: the server really does say complete here (%v,%v)", done, ok)
	}
	if st.accepted() {
		t.Fatal("a rejected mapping reported as accepted — this prints ✓ and exits 0")
	}
	if st.rejectionReason() != "could not prove checkout capability" {
		t.Errorf("reason = %q; the verifier's own text should be shown", st.rejectionReason())
	}
}

func TestEveryTerminalRejectionIsRefused(t *testing.T) {
	for state := range terminalAcceptanceStates {
		st := decodeStatus(t, `{"status":"complete","acceptance_state":"`+state+`","domain":"example.com"}`)
		if st.accepted() {
			t.Errorf("acceptance_state %q reported as a usable map", state)
		}
		if st.rejectionReason() == "" {
			t.Errorf("acceptance_state %q gives the reader no reason", state)
		}
	}
}

// Accepted but not published is not usable either: the config never reached
// the catalog, so the site still resolves as unsupported.
func TestAcceptedButUnpublishedIsNotUsable(t *testing.T) {
	for _, pub := range []string{"failed", "publishing", "pending"} {
		st := decodeStatus(t, `{"status":"complete","acceptance_state":"accepted","publication_state":"`+pub+`"}`)
		if st.accepted() {
			t.Errorf("publication_state %q reported as a usable map", pub)
		}
	}
	for _, pub := range []string{"published", "not_applicable"} {
		st := decodeStatus(t, `{"status":"complete","acceptance_state":"accepted","publication_state":"`+pub+`"}`)
		if !st.accepted() {
			t.Errorf("publication_state %q should be a usable map", pub)
		}
	}
}

// The legacy lane carries no durable request and so no acceptance_state. It
// must not start failing: there is no verdict to consult, and terminal
// "complete" is the only signal that lane has.
func TestTheLegacyLaneWithoutAVerdictStillSucceeds(t *testing.T) {
	st := decodeStatus(t, `{"status":"complete","envelope":{"domain":"example.com"}}`)
	if !st.accepted() {
		t.Fatal("a legacy complete with no acceptance_state must still count as success")
	}
	if st.site() != "example.com" {
		t.Errorf("site() = %q; the legacy nested domain must still be read", st.site())
	}
}

// The domain is TOP LEVEL. Reading it only from a nested envelope meant every
// success line said "the site" — a small wrongness that proved the struct had
// been written against a shape the endpoint never sends.
func TestTheDomainIsReadFromWhereTheServerPutsIt(t *testing.T) {
	st := decodeStatus(t, `{"status":"complete","acceptance_state":"accepted",
	                        "publication_state":"published","domain":"example.com"}`)
	if st.site() != "example.com" {
		t.Fatalf("site() = %q, want example.com (the server sends domain at top level)", st.site())
	}
	empty := decodeStatus(t, `{"status":"complete"}`)
	if empty.site() != "the site" {
		t.Errorf("site() with no domain = %q, want a readable fallback", empty.site())
	}
}

func TestAGenuinelyGoodMapStillSucceeds(t *testing.T) {
	st := decodeStatus(t, `{"status":"complete","acceptance_state":"accepted",
	                        "publication_state":"published","domain":"example.com"}`)
	done, ok := mapTerminal(st.Status)
	if !done || !ok || !st.accepted() {
		t.Fatal("a published, accepted map must report success")
	}
}
