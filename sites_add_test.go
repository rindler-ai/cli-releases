package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `rindler sites` lists what the workspace can act on; until `sites add` existed
// the CLI could only CONSUME that list, never contribute to it, so adding a site
// meant leaving the terminal for the dashboard.
func withSitesAddServer(t *testing.T, status int, capture *map[string]string) (string, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/runtime/tracked-sites" || r.Method != http.MethodPost {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if capture != nil {
			_ = json.Unmarshal(body, capture)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"id":"11111111-1111-1111-1111-111111111111","domain":"books.toscrape.com"}`))
	}))
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "credentials.json"),
		[]byte(`{"key":"rindler_live_test"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RINDLER_CONFIG_DIR", dir)
	t.Setenv("RINDLER_API_KEY", "rindler_live_test")
	return srv.URL, srv.Close
}

func TestSitesAddPostsTheDomainToTheRuntimeLane(t *testing.T) {
	var got map[string]string
	base, done := withSitesAddServer(t, http.StatusCreated, &got)
	defer done()
	if rc := runSites([]string{"add", "books.toscrape.com", "--api-base", base}); rc != 0 {
		t.Fatalf("exit=%d, want 0", rc)
	}
	if got["domain"] != "books.toscrape.com" {
		t.Errorf("wrong domain sent: %+v", got)
	}
	// `source` is provenance, not privilege: a CLI add is a requested one.
	if got["source"] != "requested" {
		t.Errorf("source should be 'requested', got %+v", got)
	}
}

// 200 means the row already existed. Reporting "added" for a no-op would state
// something the run did not do.
func TestSitesAddDistinguishesAlreadyTrackedFromAdded(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   string
	}{
		{http.StatusCreated, "Added"},
		{http.StatusOK, "already tracked"},
	} {
		base, done := withSitesAddServer(t, tc.status, nil)
		out := captureStdout(t, func() {
			if rc := runSites([]string{"add", "books.toscrape.com", "--api-base", base}); rc != 0 {
				t.Fatalf("exit non-zero for status %d", tc.status)
			}
		})
		done()
		if !strings.Contains(out, tc.want) {
			t.Errorf("status %d should report %q, got:\n%s", tc.status, tc.want, out)
		}
	}
}

func TestSitesAddRequiresADomain(t *testing.T) {
	base, done := withSitesAddServer(t, http.StatusCreated, nil)
	defer done()
	if rc := runSites([]string{"add", "--api-base", base}); rc != 2 {
		t.Errorf("a missing domain is a usage error (2), got %d", rc)
	}
}

// `sites` with no subcommand must still list, not be swallowed by the new verb.
func TestSitesStillListsWithoutTheAddSubcommand(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/runtime/configs" {
			_, _ = w.Write([]byte(`{"configs":[]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "credentials.json"), []byte(`{"key":"k"}`), 0o600)
	t.Setenv("RINDLER_CONFIG_DIR", dir)
	t.Setenv("RINDLER_API_KEY", "rindler_live_test")
	if rc := runSites([]string{"--api-base", srv.URL}); rc != 0 {
		t.Errorf("plain `sites` must still list, got exit %d", rc)
	}
}
