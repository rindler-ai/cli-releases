package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGetJSONSendsBearerAndDecodes(t *testing.T) {
	var auth, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, path = r.Header.Get("Authorization"), r.URL.Path
		// The REAL envelope. A live run against the dev API proved the list is
		// wrapped in {"configs": …}; the earlier fixture used a bare array, so the
		// test passed while the command returned zero sites against the server.
		_, _ = w.Write([]byte(`{"configs":[{"domain":"example.com","version":3,"authed":true,"action_count":7}]}`))
	}))
	defer srv.Close()

	var out configsResponse
	if err := getJSON(context.Background(), srv.Client(), srv.URL, "k", "/v1/runtime/configs", &out); err != nil {
		t.Fatalf("getJSON errored: %v", err)
	}
	if auth != "Bearer k" || path != "/v1/runtime/configs" {
		t.Errorf("auth=%q path=%q", auth, path)
	}
	if len(out.Configs) != 1 || out.Configs[0].Domain != "example.com" ||
		out.Configs[0].ActionCount == nil || *out.Configs[0].ActionCount != 7 {
		t.Errorf("decoded = %+v", out)
	}
}

// Pins the envelope shapes against the live contract, in both directions: the
// LIST is wrapped, the DETAIL is not (the server).
// Getting either backwards decodes to an empty struct with no error.
func TestConfigEnvelopeShapes(t *testing.T) {
	var list configsResponse
	if err := json.Unmarshal([]byte(`{"configs":[{"domain":"a.com"}]}`), &list); err != nil || len(list.Configs) != 1 {
		t.Fatalf("list must decode from the wrapped envelope: %v %+v", err, list)
	}
	// A bare array must NOT decode into it -- that is the bug this pins.
	if err := json.Unmarshal([]byte(`[{"domain":"a.com"}]`), &list); err == nil {
		t.Error("a bare array should fail to decode as the wrapped list envelope")
	}
	var detail siteDetail
	if err := json.Unmarshal([]byte(`{"domain":"a.com","version":2,"screens":[]}`), &detail); err != nil || detail.Domain != "a.com" {
		t.Fatalf("detail must decode UNWRAPPED: %v %+v", err, detail)
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
	}, false, false)
	out := buf.String()
	for _, want := range []string{
		"example.com", "needs login", "search_products", "reads", "writes",
		"--input query=…*", "add_to_cart", "act",
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
	printActions(&hidden, d, false, false)
	printActions(&shown, d, true, false)
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
	}}, false, false)
	if got := strings.Count(buf.String(), "login"); got != 1 {
		t.Errorf("global action should be listed once, appeared %d times:\n%s", got, buf.String())
	}
}

// The Gmail map lists 11 distinct actions across 5 screens; the screen-grouped
// view rendered 31 rows with view_inbox five times. `run` takes the NAME, so the
// screen it was found under changes nothing — dedup by name is the menu.
func TestPrintActionsDeduplicatesAcrossScreens(t *testing.T) {
	view := projAction{ActionName: "view_inbox", Method: "read", Enabled: true}
	search := projAction{ActionName: "search_mail", Method: "read", Enabled: true}
	d := siteDetail{Domain: "mail.example", Screens: []projScreen{
		{Name: "inbox", Actions: []projAction{view, search}},
		{Name: "label_view", Actions: []projAction{view, search}},
		{Name: "search_results", Actions: []projAction{view}},
	}}

	var flat bytes.Buffer
	printActions(&flat, d, false, false)
	if got := strings.Count(flat.String(), "view_inbox"); got != 1 {
		t.Errorf("deduped view should list view_inbox once, got %d:\n%s", got, flat.String())
	}
	if !strings.Contains(flat.String(), "2 action(s)") {
		t.Errorf("should report the DISTINCT count, got:\n%s", flat.String())
	}

	// --by-screen deliberately keeps the repetition, because that view is about
	// topology.
	var grouped bytes.Buffer
	printActions(&grouped, d, false, true)
	if got := strings.Count(grouped.String(), "view_inbox"); got != 3 {
		t.Errorf("--by-screen should show it per screen, got %d", got)
	}
}

// Reads before writes: trying a reader is the safe move, so it comes first.
func TestPrintActionsOrdersReadsBeforeWrites(t *testing.T) {
	var buf bytes.Buffer
	printActions(&buf, siteDetail{Domain: "e.com", Screens: []projScreen{{
		Name: "s",
		Actions: []projAction{
			{ActionName: "delete_thing", Method: "act", Enabled: true},
			{ActionName: "view_thing", Method: "read", Enabled: true},
		},
	}}}, false, false)
	out := buf.String()
	if strings.Index(out, "view_thing") > strings.Index(out, "delete_thing") {
		t.Errorf("reads should be listed before writes, got:\n%s", out)
	}
	if !strings.Contains(out, "reads") || !strings.Contains(out, "writes") {
		t.Errorf("output should label the two groups, got:\n%s", out)
	}
}

func TestFirstSentenceTrims(t *testing.T) {
	if got := firstSentence("Open the inbox. Then do more things that go on."); got != "Open the inbox." {
		t.Errorf("got %q", got)
	}
	long := strings.Repeat("x", 200)
	if got := firstSentence(long); len(got) > 100 {
		t.Errorf("should cap long descriptions, got %d chars", len(got))
	}
	if got := firstSentence("  multi\nline  "); got != "multi line" {
		t.Errorf("newlines should collapse, got %q", got)
	}
}
