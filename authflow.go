package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"
)

// Client-side pieces of the OAuth Authorization Code + PKCE login flow.

const (
	cliClientID = "rindler-cli"
	// Default hosts (overridable by flags/env for dev). AuthorizeBase serves the
	// dashboard consent page; APIBase serves the public /api/cli/token exchange.
	defaultAuthorizeBase = "https://app.rindler.ai"
	defaultAPIBase       = "https://mcp.rindler.ai"
)

// tokenResponse is the /api/cli/token success body.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	Last4        string `json:"last4"`
	ExpiresAt    string `json:"expires_at"`
	MapperAccess bool   `json:"mapper_access"`
	ClerkUserID  string `json:"clerk_user_id"`
	AccountEmail string `json:"account_email"`
	MCPURL       string `json:"mcp_url"`
}

// oauthError is the RFC 6749 §5.2 error body.
type oauthError struct {
	Err  string `json:"error"`
	Desc string `json:"error_description"`
}

func (e oauthError) Error() string {
	if e.Desc != "" {
		return e.Err + ": " + e.Desc
	}
	return e.Err
}

// buildAuthorizeURL constructs the dashboard consent URL the browser opens. When
// paste is true it adds paste=1 so the dashboard renders the code#state page
// instead of redirecting to a loopback port.
func buildAuthorizeURL(authorizeBase, redirectURI string, p pkce, device string, mapping, paste bool) string {
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", cliClientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", "mcp")
	q.Set("code_challenge", p.Challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("state", p.State)
	q.Set("device", device)
	if mapping {
		q.Set("mapping_requested", "1")
	}
	if paste {
		q.Set("paste", "1")
	}
	return strings.TrimRight(authorizeBase, "/") + "/cli/authorize?" + q.Encode()
}

// deviceLabel builds a human label for the key (host + os + user).
func deviceLabel() string {
	host, _ := os.Hostname()
	user := os.Getenv("USER")
	if user == "" {
		user = os.Getenv("USERNAME")
	}
	label := strings.TrimSpace(host)
	if label == "" {
		label = "unknown-host"
	}
	label += " (" + runtime.GOOS
	if user != "" {
		label += ", " + user
	}
	label += ")"
	return label
}

// splitPastedCode parses a "<code>#<state>" paste. A bare code (no '#') is
// returned with an empty state (state check then skipped for that path).
func splitPastedCode(s string) (code, state string) {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "#"); i >= 0 {
		return s[:i], s[i+1:]
	}
	return s, ""
}

// exchangeToken POSTs the authorization code + PKCE verifier to /api/cli/token
// and returns the minted key. It returns an oauthError for a structured error
// body, else a generic error.
func exchangeToken(ctx context.Context, httpc *http.Client, apiBase, code, verifier, redirectURI string) (tokenResponse, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("code_verifier", verifier)
	form.Set("redirect_uri", redirectURI)
	form.Set("client_id", cliClientID)

	endpoint := strings.TrimRight(apiBase, "/") + "/api/cli/token"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return tokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := httpc.Do(req)
	if err != nil {
		return tokenResponse{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode != http.StatusOK {
		var oe oauthError
		if json.Unmarshal(body, &oe) == nil && oe.Err != "" {
			return tokenResponse{}, oe
		}
		return tokenResponse{}, fmt.Errorf("token exchange failed: HTTP %d", resp.StatusCode)
	}
	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return tokenResponse{}, fmt.Errorf("malformed token response: %w", err)
	}
	if tr.AccessToken == "" {
		return tokenResponse{}, fmt.Errorf("token response missing access_token")
	}
	return tr, nil
}

// defaultHTTPClient is a bounded client for the token exchange.
func defaultHTTPClient() *http.Client {
	return &http.Client{Timeout: 30 * time.Second}
}

// revokeOutcome is what happened server-side at logout. THREE states, because
// the server distinguishes two successes and conflating them makes one of them
// read as a failure.
type revokeOutcome int

const (
	// revokeUnreachable: we could not tell the server. Local state is still
	// cleared; the key expires with the Clerk session anyway.
	revokeUnreachable revokeOutcome = iota
	// revokeDone: the server retired a live key.
	revokeDone
	// revokeNothingToDo: the server answered fine and had nothing to revoke,
	// because the key had already lapsed with its Clerk session. That is the
	// DOMINANT case for anyone who has been away a while, and it is a success:
	// the key is gone, which is what was asked for.
	revokeNothingToDo
)

// revokeSelf best-effort revokes the presented key server-side (logout). It POSTs
// the key as a Bearer to /api/cli/logout; a 404 (endpoint not yet deployed) or
// any transport error is returned so the caller can note it, but logout still
// clears local state regardless.
//
// The response is 200 {"ok":true,"revoked":<bool>}, and `revoked` is false
// whenever there was nothing live to retire. Reading only the STATUS reported
// "✓ Key revoked server-side" for a key that had already expired -- true in
// outcome, false in what it claimed to have done -- and treating false as a
// failure instead would print a warning for the most common healthy case.
func revokeSelf(ctx context.Context, httpc *http.Client, apiBase, key string) (revokeOutcome, error) {
	endpoint := strings.TrimRight(apiBase, "/") + "/api/cli/logout"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return revokeUnreachable, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := httpc.Do(req)
	if err != nil {
		return revokeUnreachable, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return revokeUnreachable, nil
	}
	// An unreadable or absent `revoked` is treated as DONE rather than
	// nothing-to-do: the server said 2xx, and the difference between the two
	// successes only changes a sentence.
	var out struct {
		Revoked *bool `json:"revoked"`
	}
	if json.Unmarshal(body, &out) == nil && out.Revoked != nil && !*out.Revoked {
		return revokeNothingToDo, nil
	}
	return revokeDone, nil
}
