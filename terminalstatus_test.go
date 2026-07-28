package main

import "testing"

// The CLI's terminal-status sets must cover everything the SERVER treats as
// finished. A status the server has finalised but the CLI does not recognise
// is an infinite poll, not a slow one: the job is over, so its status will
// never change again, and the CLI waits out its whole --timeout before
// reporting a timeout that never happened.
//
// serverTerminal is transcribed from the server's own terminal-status
// predicate, which is the authoritative set. Keep it in sync deliberately,
// not by memory.
var serverTerminal = []string{
	"complete", "error", "failed", "expired", "cancelled", "needs_escalation",
}

func TestMapTerminalCoversEveryServerTerminalStatus(t *testing.T) {
	for _, s := range serverTerminal {
		if done, _ := mapTerminal(s); !done {
			t.Errorf("mapTerminal(%q) says keep polling, but the server has finished the job — this hangs until --timeout", s)
		}
	}
}

func TestRunTerminalCoversEveryServerTerminalStatus(t *testing.T) {
	for _, s := range serverTerminal {
		if done, _ := runTerminal(s); !done {
			t.Errorf("runTerminal(%q) says keep polling, but the server has finished the job", s)
		}
	}
}

// Terminal is not the same as successful. Reporting an aged-out or escalated
// job as a success would be the worse failure of the two.
func TestOnlyRealSuccessesReportSuccess(t *testing.T) {
	for _, s := range []string{"expired", "needs_escalation", "failed", "error", "cancelled"} {
		if _, ok := mapTerminal(s); ok {
			t.Errorf("mapTerminal(%q) reports SUCCESS", s)
		}
		if _, ok := runTerminal(s); ok {
			t.Errorf("runTerminal(%q) reports SUCCESS", s)
		}
	}
	for _, s := range []string{"complete", "succeeded"} {
		if done, ok := mapTerminal(s); !done || !ok {
			t.Errorf("mapTerminal(%q) = (%v,%v), want a success", s, done, ok)
		}
	}
}

// An unknown status must NOT be treated as finished: a new intermediate state
// should be waited through rather than guessed at. Safe only because the
// caller bounds the wait.
func TestAnUnknownStatusKeepsPolling(t *testing.T) {
	for _, s := range []string{"queued", "running", "pending", "some_new_state", ""} {
		if done, _ := mapTerminal(s); done {
			t.Errorf("mapTerminal(%q) stopped polling on a non-terminal status", s)
		}
		if done, _ := runTerminal(s); done {
			t.Errorf("runTerminal(%q) stopped polling on a non-terminal status", s)
		}
	}
}

// A terminal failure the server sends with no message must still explain
// itself, or the reader goes hunting for a mistake in their own input.
func TestAgedOutAndEscalatedJobsExplainThemselves(t *testing.T) {
	for _, s := range []string{"expired", "needs_escalation"} {
		if mapStatusExplanation(s) == "" {
			t.Errorf("%q terminates with no explanation", s)
		}
	}
	if mapStatusExplanation("failed") != "" {
		t.Error(`"failed" needs no invented explanation; the server's message is the reason`)
	}
}
