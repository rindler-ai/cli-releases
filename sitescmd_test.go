package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGetJSONSendsBearerAndDecodes(t *testing.T) {
	var auth, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, path = r.Header.Get("Authorization"), r.URL.Path
		_, _ = w.Write([]byte(`[{"domain":"example.com","version":3,"authed":true,"action_count":7}]`))
	}))
	defer srv.Close()

	var out []siteSummary
	if err := getJSON(context.Background(), srv.Client(), srv.URL, "k", "/v1/runtime/configs", &out); err != nil {
		t.Fatalf("getJSON errored: %v", err)
	}
	if auth != "Bearer k" || path != "/v1/runtime/configs" {
		t.Errorf("auth=%q path=%q", auth, path)
	}
	if len(out) != 1 || out[0].Domain != "example.com" || out[0].ActionCount == nil || *out[0].ActionCount != 7 {
		t.Errorf("decoded = %+v", out)
	}
}

func TestGetJSONSurfacesHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	var out []siteSummary
	err := getJSON(context.Background(), srv.Client(), srv.URL, "k", "/v1/runtime/configs", &out)
	if err == nil || !strings.Contains(err.Error(), "rindler login") {
		t.Fatalf("a 401 should point at login, got %v", err)
	}
}

// action_count is a POINTER on purpose: absent means "we could not read the
// config body", which is a different fact from zero actions. Rendering absent as
// 0 would tell the user the site is mapped-but-empty.
func TestPrintActionsDistinguishesAbsentFromZero(t *testing.T) {
	n := 0
	withZero := siteSummary{Domain: "a.com", ActionCount: &n}
	withAbsent := siteSummary{Domain: "b.com"}
	if withZero.ActionCount == nil {
		t.Fatal("fixture wrong")
	}
	if withAbsent.ActionCount != nil {
		t.Fatal("absent count must stay nil")
	}
}

func TestPrintActionsRendersRunnableNames(t *testing.T) {
	var buf bytes.Buffer
	printActions(&buf, siteDetail{
		Domain: "example.com", Version: 2, Authed: true,
		Screens: []projScreen{{
			Name: "search",
			Actions: []projAction{
				{ActionName: "search_products", Method: "read", Enabled: true,
					Params:      []projParam{{Name: "query", Required: true}},
					Description: "Search the catalog."},
				{ActionName: "add_to_cart", Method: "act", Enabled: true},
			},
		}},
	}, false)
	out := buf.String()
	for _, want := range []string{
		"example.com", "needs login", "search_products", "read",
		"--input query=… (required)", "add_to_cart", "act",
		"rindler run --site example.com",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("actions output should contain %q, got:\n%s", want, out)
		}
	}
}

func TestPrintActionsHidesDisabledUnlessAsked(t *testing.T) {
	d := siteDetail{Domain: "e.com", Screens: []projScreen{{
		Name:    "s",
		Actions: []projAction{{ActionName: "gone", Enabled: false}},
	}}}
	var hidden, shown bytes.Buffer
	printActions(&hidden, d, false)
	printActions(&shown, d, true)
	if strings.Contains(hidden.String(), "gone") {
		t.Error("a disabled action must be hidden by default")
	}
	if !strings.Contains(shown.String(), "gone") || !strings.Contains(shown.String(), "disabled") {
		t.Errorf("--all must show it and mark it disabled, got:\n%s", shown.String())
	}
	if !strings.Contains(hidden.String(), "No enabled actions") {
		t.Errorf("an all-disabled site should say so, got:\n%s", hidden.String())
	}
}

// A global action is attached to every screen by the server. Listing it once
// keeps the output a menu instead of a repeated transcript.
func TestPrintActionsDeduplicatesGlobals(t *testing.T) {
	global := projAction{ActionName: "login", Method: "act", Enabled: true, Global: true}
	var buf bytes.Buffer
	printActions(&buf, siteDetail{Domain: "e.com", Screens: []projScreen{
		{Name: "one", Actions: []projAction{global}},
		{Name: "two", Actions: []projAction{global}},
	}}, false)
	if got := strings.Count(buf.String(), "login"); got != 1 {
		t.Errorf("global action should be listed once, appeared %d times:\n%s", got, buf.String())
	}
}
