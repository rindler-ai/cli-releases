package main

import (
	"strings"
	"testing"
)

// A CLI key carries TWO identities under MODEL B: the ACTOR who signed in and
// the SCOPE (workspace owner) the key acts within. whoami used to pair the
// actor's email with the scope id, so a member of someone else's workspace saw
// "member@corp.com (user_<owner>)" — an email and an id belonging to two
// different people. These cases pin the pairing to like-with-like.
func TestWhoamiLines(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  cliConfig
		want []string
	}{
		{
			// The regression: actor and scope differ.
			name: "member of another workspace",
			cfg: cliConfig{
				Email:              "member@corp.com",
				AccountClerkUserID: "user_member",
				ClerkUserID:        "user_owner",
			},
			want: []string{"member@corp.com (user_member)", "workspace: user_owner"},
		},
		{
			// The common case: personal workspace, one identity, no extra noise.
			name: "own workspace",
			cfg: cliConfig{
				Email:              "solo@corp.com",
				AccountClerkUserID: "user_solo",
				ClerkUserID:        "user_solo",
			},
			want: []string{"solo@corp.com (user_solo)"},
		},
		{
			// Server predates account_clerk_user_id: we hold only the scope, so
			// print the email alone and label the workspace rather than imply the
			// address owns that id.
			name: "old server, email but no actor id",
			cfg: cliConfig{
				Email:       "member@corp.com",
				ClerkUserID: "user_owner",
			},
			want: []string{"member@corp.com", "workspace: user_owner"},
		},
		{
			// Scope is the only identity we hold: print it plainly, with no
			// contrasting "workspace:" line to label it against.
			name: "scope only",
			cfg:  cliConfig{ClerkUserID: "user_owner"},
			want: []string{"user_owner"},
		},
		{
			name: "nothing but a key",
			cfg:  cliConfig{Last4: "ab12"},
			want: []string{"logged in (key …ab12)"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := whoamiLines(tc.cfg)
			if strings.Join(got, "\n") != strings.Join(tc.want, "\n") {
				t.Errorf("whoamiLines()\n got: %q\nwant: %q", got, tc.want)
			}
		})
	}
}

// The precise defect, stated as an invariant: no single line may pair an email
// with a Clerk id that is not that email's own account.
func TestWhoamiNeverPairsEmailWithForeignID(t *testing.T) {
	cfg := cliConfig{
		Email:              "member@corp.com",
		AccountClerkUserID: "user_member",
		ClerkUserID:        "user_owner",
	}
	for _, line := range whoamiLines(cfg) {
		if strings.Contains(line, cfg.Email) && strings.Contains(line, cfg.ClerkUserID) {
			t.Fatalf("line pairs the signer's email with the workspace owner's id: %q", line)
		}
	}
}
