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

// A site the catalog already has is reusable from a bare domain, but a site we
// have never mapped needs a FRESH CRAWL, and the server refuses to start one
// from a normalized host: a bare domain may have discarded the tenant or vendor
// path that identifies the real target. So send the URL the caller typed, when
// they typed one, rather than reconstructing https://<host> and pretending it
// was given.
func TestSitesAddForwardsAFullURLVerbatim(t *testing.T) {
	var got map[string]string
	base, done := withSitesAddServer(t, http.StatusCreated, &got)
	defer done()
	target := "https://books.toscrape.com/catalogue/category/books/travel_2/index.html"
	if rc := runSites([]string{"add", target, "--api-base", base}); rc != 0 {
		t.Fatalf("exit=%d, want 0", rc)
	}
	if got["url"] != target {
		t.Errorf("the full target must be forwarded verbatim, got %q", got["url"])
	}
	if got["domain"] != "books.toscrape.com" {
		t.Errorf("domain should still be the host: %+v", got)
	}
}

// A bare host carries no path to preserve, so send no url and let the server
// decide: it reuses a catalog config, or refuses the fresh crawl.
func TestSitesAddSendsNoURLForABareHost(t *testing.T) {
	var got map[string]string
	base, done := withSitesAddServer(t, http.StatusCreated, &got)
	defer done()
	if rc := runSites([]string{"add", "books.toscrape.com", "--api-base", base}); rc != 0 {
		t.Fatalf("exit=%d, want 0", rc)
	}
	if _, present := got["url"]; present {
		t.Errorf("a bare host must not fabricate a url: %+v", got)
	}
}

// The one refusal a user can act on. Forwarding the server's phrasing reads like
// the CLI forgot a field; say what to type instead.
func TestSitesAddExplainsTheFreshCrawlURLRequirement(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"url is required for a new mapping request"}`))
	}))
	defer srv.Close()
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "credentials.json"), []byte(`{"key":"k"}`), 0o600)
	t.Setenv("RINDLER_CONFIG_DIR", dir)
	t.Setenv("RINDLER_API_KEY", "rindler_live_test")

	out := captureStderr(t, func() {
		if rc := runSites([]string{"add", "books.toscrape.com", "--api-base", srv.URL}); rc != 1 {
			t.Errorf("a refusal is exit 1")
		}
	})
	if !strings.Contains(out, "rindler sites add https://books.toscrape.com/") {
		t.Errorf("the message must show the command to type, got:\n%s", out)
	}
}

// captureStderr mirrors captureStdout for the refusal paths, which write to
// stderr so a script's stdout stays parseable.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stderr
	os.Stderr = w
	done := make(chan string, 1)
	go func() {
		var b strings.Builder
		_, _ = io.Copy(&b, r)
		done <- b.String()
	}()
	fn()
	os.Stderr = saved
	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out
}
