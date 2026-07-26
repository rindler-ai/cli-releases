package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Dispatcher-level tests: they call run() with real argv and a fake server, so
// they cover the command entry points and the arg routing a user actually hits.
// The narrower unit tests cover the helpers; these prove the wiring between
// them, which is where a command silently does nothing.

// isolate points every path the CLI writes at a temp dir and supplies a key via
// the environment, so no test touches a real keyring, agent config, or account.
func isolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("RINDLER_CONFIG_DIR", filepath.Join(dir, "rindler"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(dir, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(dir, "codex"))
	t.Setenv("HOME", dir)
	if err := os.MkdirAll(filepath.Join(dir, "claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// fakeAPI serves the endpoints the CLI reads, in the REAL envelope shapes.
func fakeAPI(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/runtime/configs", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"configs":[{"domain":"example.com","version":2,"authed":false,"action_count":3}]}`))
	})
	mux.HandleFunc("GET /v1/runtime/configs/{domain}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("domain") != "example.com" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"class":"config_not_found"}`))
			return
		}
		_, _ = w.Write([]byte(`{"domain":"example.com","version":2,"screens":[
		  {"name":"search","actions":[
		    {"action_name":"search_products","method":"read","enabled":true,
		     "params":[{"name":"query","required":true}],"description":"Search."}]}]}`))
	})
	mux.HandleFunc("POST /v1/runtime/run", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["site"] != "example.com" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"job_id":"job-1"}`))
	})
	mux.HandleFunc("GET /v1/runtime/jobs/{id}", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"complete","usage":{"outcome_count":1},
		  "outputs":{"records":[{"title":"Shoe"}]},"retrieval":{"outcome":"records","complete":true}}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestDispatchUnknownCommandFails(t *testing.T) {
	isolate(t)
	if code := run([]string{"definitely-not-a-command"}); code != 2 {
		t.Errorf("unknown command should exit 2, got %d", code)
	}
	if code := run(nil); code != 2 {
		t.Errorf("no args should exit 2, got %d", code)
	}
}

func TestDispatchHelpAndVersion(t *testing.T) {
	isolate(t)
	for _, args := range [][]string{{"help"}, {"--help"}, {"-h"}, {"version"}, {"--version"}, {"-v"}} {
		if code := run(args); code != 0 {
			t.Errorf("%v should exit 0, got %d", args, code)
		}
	}
}

func TestSitesEndToEnd(t *testing.T) {
	isolate(t)
	srv := fakeAPI(t)
	t.Setenv("RINDLER_API_KEY", "rindler_live_test")
	if code := run([]string{"sites", "--api-base", srv.URL}); code != 0 {
		t.Fatalf("sites should exit 0, got %d", code)
	}
}

func TestActionsEndToEndWithFlagAfterPositional(t *testing.T) {
	isolate(t)
	srv := fakeAPI(t)
	t.Setenv("RINDLER_API_KEY", "rindler_live_test")
	// Flag AFTER the positional — the ordering that used to silently target prod.
	if code := run([]string{"actions", "example.com", "--api-base", srv.URL}); code != 0 {
		t.Fatalf("actions should exit 0, got %d", code)
	}
	// An unknown site must fail, not quietly print an empty surface.
	if code := run([]string{"actions", "nope.com", "--api-base", srv.URL}); code == 0 {
		t.Error("actions on an unmapped site should exit non-zero")
	}
}

func TestRunEndToEnd(t *testing.T) {
	isolate(t)
	srv := fakeAPI(t)
	t.Setenv("RINDLER_API_KEY", "rindler_live_test")
	code := run([]string{"run", "--site", "example.com", "--action", "search_products",
		"--input", "query=shoes", "--api-base", srv.URL})
	if code != 0 {
		t.Fatalf("run should exit 0 on a complete job with records, got %d", code)
	}
}

func TestRunRequiresSiteAndAction(t *testing.T) {
	isolate(t)
	t.Setenv("RINDLER_API_KEY", "rindler_live_test")
	if code := run([]string{"run", "--site", "example.com"}); code != 2 {
		t.Errorf("run without --action should be a usage error, got %d", code)
	}
	if code := run([]string{"run", "--action", "x"}); code != 2 {
		t.Errorf("run without --site should be a usage error, got %d", code)
	}
}

func TestRunStatusEndToEnd(t *testing.T) {
	isolate(t)
	srv := fakeAPI(t)
	t.Setenv("RINDLER_API_KEY", "rindler_live_test")
	if code := run([]string{"run", "status", "job-1", "--once", "--api-base", srv.URL}); code != 0 {
		t.Fatalf("run status --once should exit 0, got %d", code)
	}
}

// Every verb must refuse clearly when there is no credential, rather than
// erroring somewhere deeper with a confusing message.
func TestVerbsRequireLogin(t *testing.T) {
	isolate(t)
	os.Unsetenv("RINDLER_API_KEY")
	for _, args := range [][]string{
		{"sites"},
		{"actions", "example.com"},
		{"run", "--site", "example.com", "--action", "x"},
		{"run", "status", "job-1"},
		{"map", "https://example.com"},
		{"map", "status", "job-1"},
	} {
		if code := run(args); code == 0 {
			t.Errorf("%v should not exit 0 when logged out", args)
		}
	}
}

func TestWhoamiFailsLoggedOutAndPassesWithEnvKey(t *testing.T) {
	isolate(t)
	os.Unsetenv("RINDLER_API_KEY")
	if code := run([]string{"whoami"}); code == 0 {
		t.Error("whoami should exit non-zero when logged out")
	}
	t.Setenv("RINDLER_API_KEY", "rindler_live_test")
	if code := run([]string{"whoami"}); code != 0 {
		t.Error("whoami should exit 0 with an env key")
	}
}

func TestStatusAlwaysSucceeds(t *testing.T) {
	isolate(t)
	os.Unsetenv("RINDLER_API_KEY")
	// status is a report, not a gate: it must describe a logged-out machine
	// rather than failing on it.
	if code := run([]string{"status"}); code != 0 {
		t.Errorf("status should exit 0 even logged out, got %d", code)
	}
}

func TestDoctorFailsLoggedOutAndIsOfflineCapable(t *testing.T) {
	isolate(t)
	os.Unsetenv("RINDLER_API_KEY")
	if code := run([]string{"doctor", "--offline"}); code != 1 {
		t.Errorf("doctor should exit 1 when not logged in, got %d", code)
	}
}

// The MCP install lifecycle, against temp agent configs.
func TestMCPInstallStatusRemoveLifecycle(t *testing.T) {
	dir := isolate(t)
	t.Setenv("RINDLER_API_KEY", "rindler_live_test")

	if code := run([]string{"mcp", "install"}); code != 0 {
		t.Fatalf("mcp install should exit 0, got %d", code)
	}
	claudeCfg := filepath.Join(dir, "claude", ".claude.json")
	b, err := os.ReadFile(claudeCfg)
	if err != nil {
		t.Fatalf("claude config not written: %v", err)
	}
	if !strings.Contains(string(b), "rindler") {
		t.Errorf("claude config should carry the rindler server, got %s", b)
	}
	// The key must be written as the Bearer, since that is what makes the MCP work.
	if !strings.Contains(string(b), "rindler_live_test") {
		t.Errorf("claude config should carry the key as a bearer")
	}

	if code := run([]string{"mcp", "status"}); code != 0 {
		t.Errorf("mcp status should exit 0, got %d", code)
	}
	if code := run([]string{"mcp", "remove"}); code != 0 {
		t.Errorf("mcp remove should exit 0, got %d", code)
	}
	b2, err := os.ReadFile(claudeCfg)
	if err != nil {
		t.Fatalf("claude config vanished on remove: %v", err)
	}
	if strings.Contains(string(b2), "rindler_live_test") {
		t.Errorf("remove must drop the key, got %s", b2)
	}
}

// `rindler install` is deliberately NOT a command: on a CLI, a bare "install"
// reads as installing the tool itself, not an MCP server into an agent.
// `rindler mcp install` is the only spelling.
func TestBareInstallIsNotACommand(t *testing.T) {
	isolate(t)
	if code := run([]string{"install", "mcp"}); code != 2 {
		t.Errorf("`install` should be an unknown command, got exit %d", code)
	}
	if code := run([]string{"install"}); code != 2 {
		t.Errorf("bare `install` should be an unknown command, got exit %d", code)
	}
}

func TestMCPUnknownSubcommand(t *testing.T) {
	isolate(t)
	if code := run([]string{"mcp", "wat"}); code != 2 {
		t.Errorf("unknown mcp subcommand should exit 2, got %d", code)
	}
	if code := run([]string{"mcp"}); code != 2 {
		t.Errorf("bare `mcp` should exit 2, got %d", code)
	}
}
