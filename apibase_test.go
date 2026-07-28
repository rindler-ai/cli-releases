package main

import "testing"

// RINDLER_API_BASE is advertised in the help text as "override the API origin",
// with no verb qualifier. It must therefore work for every command, not just
// login.
//
// The stakes are asymmetric. When the override is ignored the fallback is the
// PRODUCTION default, and the case where the variable matters most -- CI, where
// you set RINDLER_API_KEY and never log in, so there is no config to fall back
// to -- is exactly the case that landed on prod.
func TestApiBaseResolutionOrder(t *testing.T) {
	logged := cliConfig{APIBase: "https://from-login.example"}
	for _, tc := range []struct {
		name, flag, env string
		cfg             cliConfig
		want            string
	}{
		{"flag wins over everything", "https://flag.example", "https://env.example", logged, "https://flag.example"},
		{"env beats a stale login config", "", "https://env.example", logged, "https://env.example"},
		{"config when no flag or env", "", "", logged, "https://from-login.example"},
		{"default when nothing is set", "", "", cliConfig{}, defaultAPIBase},
		// THE CI CASE: no config at all, only the env var.
		{"env alone, never logged in", "", "https://staging.example", cliConfig{}, "https://staging.example"},
		// Whitespace and trailing slashes must not defeat a real value and fall
		// through to prod.
		{"trailing slash trimmed", "", "https://env.example/", cliConfig{}, "https://env.example"},
		{"blank env is not a value", "", "   ", logged, "https://from-login.example"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("RINDLER_API_BASE", tc.env)
			if got := resolveAPIBase(tc.flag, tc.cfg); got != tc.want {
				t.Errorf("resolveAPIBase(%q, cfg=%q) with env %q = %q, want %q",
					tc.flag, tc.cfg.APIBase, tc.env, got, tc.want)
			}
		})
	}
}

// Every authenticated command must resolve through the SAME function, or the
// override works for some verbs and not others -- which is how this broke.
func TestEveryCommandSeesTheOverride(t *testing.T) {
	isolate(t)
	t.Setenv("RINDLER_API_KEY", "rindler_live_test")
	t.Setenv("RINDLER_API_BASE", "https://staging.example")

	_, base, code := resolveKeyAndBase("", "test")
	if code != 0 || base != "https://staging.example" {
		t.Errorf("resolveKeyAndBase = %q (exit %d), want the override", base, code)
	}
	_, qbase, qcode := resolveKeyAndBaseQuiet("")
	if qcode != 0 || qbase != "https://staging.example" {
		t.Errorf("resolveKeyAndBaseQuiet = %q (exit %d), want the override", qbase, qcode)
	}
	// doctor must diagnose the lane the commands actually use; disagreeing would
	// have it confidently describe an origin you are not talking to.
	if base != qbase {
		t.Errorf("doctor resolves %q but the commands resolve %q", qbase, base)
	}
}
