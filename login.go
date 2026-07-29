package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// The rindler login flow: OAuth 2.0 Authorization Code + PKCE, with a
// loopback redirect (primary) and a hosted paste-code fallback.

type loginOpts struct {
	AuthorizeBase string
	APIBase       string
	Mapping       bool
	Device        string
}

// callbackResult is what the loopback handler captures from the redirect.
type callbackResult struct {
	code   string
	state  string
	errMsg string
}

// loopbackLogin runs the same-machine flow: bind a loopback listener, open the
// browser, capture the redirect, and exchange the code.
func loopbackLogin(ctx context.Context, opts loginOpts, p pkce, httpc *http.Client, openFn func(string) error) (tokenResponse, error) {
	// Bind the LITERAL loopback (never "localhost", never :0 on all interfaces) so
	// the code returns only to this machine (RFC 8252).
	lc := net.ListenConfig{}
	ln, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return tokenResponse{}, fmt.Errorf("could not open a local callback port: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	redirect := fmt.Sprintf("http://127.0.0.1:%d/callback", port)

	resultCh := make(chan callbackResult, 1)
	srv := &http.Server{Handler: loopbackHandler(resultCh)}
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()

	authURL := buildAuthorizeURL(opts.AuthorizeBase, redirect, p, opts.Device, opts.Mapping, false)
	fmt.Println("Opening your browser to sign in:")
	fmt.Println("  " + authURL)
	if err := openFn(authURL); err != nil {
		fmt.Println("(couldn't open a browser automatically — open the URL above yourself)")
	}
	fmt.Println("Waiting for approval in the browser…")

	select {
	case res := <-resultCh:
		if res.errMsg != "" {
			return tokenResponse{}, fmt.Errorf("authorization was denied: %s", res.errMsg)
		}
		if res.state != p.State {
			return tokenResponse{}, errors.New("state mismatch (possible CSRF) — aborting")
		}
		return exchangeToken(ctx, httpc, opts.APIBase, res.code, p.Verifier, redirect)
	case <-ctx.Done():
		return tokenResponse{}, errors.New("timed out waiting for browser approval")
	}
}

// loopbackHandler serves the /callback redirect and hands the result to ch.
func loopbackHandler(ch chan callbackResult) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/callback" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		res := callbackResult{code: q.Get("code"), state: q.Get("state"), errMsg: q.Get("error")}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if res.errMsg != "" || res.code == "" {
			_, _ = io.WriteString(w, resultPage("Sign-in failed", "Return to your terminal and try again."))
		} else {
			_, _ = io.WriteString(w, resultPage("Signed in to Rindler", "You can close this tab and return to your terminal."))
		}
		select {
		case ch <- res:
		default:
		}
	})
}

func resultPage(title, body string) string {
	return "<!doctype html><html><head><meta charset=utf-8><title>" + title +
		"</title><style>body{font-family:system-ui,sans-serif;max-width:32rem;margin:15vh auto;text-align:center;color:#111}h1{font-size:1.4rem}</style></head><body><h1>" +
		title + "</h1><p>" + body + "</p></body></html>"
}

// pasteLogin runs the different-browser / headless flow: print the URL, read the
// pasted code#state, and exchange.
// pasteCodeLifetime mirrors the server's cliAuthCodeTTL (60s). Stated to the
// user because the window is short enough to miss while walking to a phone.
const pasteCodeLifetime = "a minute"

func pasteLogin(ctx context.Context, opts loginOpts, p pkce, httpc *http.Client, openFn func(string) error, prompt func(string) (string, error)) (tokenResponse, error) {
	redirect := strings.TrimRight(opts.AuthorizeBase, "/") + "/cli/complete"
	authURL := buildAuthorizeURL(opts.AuthorizeBase, redirect, p, opts.Device, opts.Mapping, true)
	fmt.Println("Open this URL in a browser on any device to sign in:")
	fmt.Println("  " + authURL)
	_ = openFn(authURL) // best-effort; the user may be on another machine
	// The code expires in a minute server-side. Saying so is not decoration: the
	// window is tight for "open this on your phone, sign in, approve, come back",
	// and without the warning an expiry reads as a broken login rather than a
	// slow one.
	fmt.Printf("\nThe code is only valid for about %s after it appears, so have this\n", pasteCodeLifetime)
	fmt.Println("terminal ready before you approve.")

	pasted, err := prompt("\nPaste the code shown after you approve: ")
	if err != nil {
		return tokenResponse{}, err
	}
	code, state := splitPastedCode(pasted)
	if code == "" {
		return tokenResponse{}, errors.New("no code entered")
	}
	// STATE IS REQUIRED, not merely checked when present.
	//
	// The dashboard will not render a code without one -- /cli/complete refuses
	// when either half is missing, and /cli/authorize refuses a request with no
	// state at all -- so a stateless paste is never something this flow produced.
	// It is a truncated copy, or a bare code somebody else supplied.
	//
	// Skipping the check on an absent state, which is what this did, is the
	// weaker half of a CSRF guard: an attacker who can get a victim to paste a
	// code they chose only has to omit the fragment to bypass it entirely.
	if state == "" {
		return tokenResponse{}, errors.New(
			"that code is missing its verification part — copy the WHOLE value, including everything after the '#'")
	}
	if state != p.State {
		return tokenResponse{}, errors.New("state mismatch (possible CSRF) — aborting")
	}
	return exchangeToken(ctx, httpc, opts.APIBase, code, p.Verifier, redirect)
}

// promptStdin reads a single trimmed line from stdin.
func promptStdin(msg string) (string, error) {
	fmt.Print(msg)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// runLogin is the `rindler login` command.
func runLogin(args []string) int {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	paste := fs.Bool("paste", false, "use the paste-a-code flow instead of the loopback redirect (for a different browser / headless / SSH)")
	// Mapping is requested BY DEFAULT. It used to be opt-in via --map, which was a
	// footgun: the server always writes the mapper_access column, so a plain
	// `rindler login` minted a key with mapper_access=FALSE, and the authorization
	// check reads that flag FIRST and short-circuits -- so the key was affirmatively
	// DENIED the mapper, with no fallback to the caller's admin status. You got a
	// login that silently could not map, and nothing said why. Requesting it always
	// is safe because the server grants it only if the workspace is entitled, so a
	// non-entitled login still comes back false; it just is not false by accident.
	noMap := fs.Bool("no-map", false, "do not request site-mapping capability")
	deprecatedMap := fs.Bool("map", false, "deprecated: mapping is requested by default; this flag is a no-op")
	noMCP := fs.Bool("no-mcp", false, "do not install the MCP into Claude Code / Codex after login")
	authorizeBase := fs.String("authorize-base", envOr("RINDLER_AUTHORIZE_BASE", defaultAuthorizeBase), "dashboard origin serving the consent page")
	apiBase := fs.String("api-base", envOr("RINDLER_API_BASE", defaultAPIBase), "Rindler API origin serving /api/cli/token")
	timeout := fs.Duration("timeout", 5*time.Minute, "how long to wait for browser approval")
	if err := fs.Parse(args); err != nil {
		// `--help` is a successful request for help, not a usage error: exiting 2
		// to stderr makes `rindler login --help | less` print nothing and trips any
		// wrapper that treats nonzero as failure.
		if errors.Is(err, flag.ErrHelp) {
			fs.SetOutput(os.Stdout)
			fs.Usage()
			return 0
		}
		return 2
	}

	p, err := newPKCE()
	if err != nil {
		fmt.Fprintln(os.Stderr, "login failed:", err)
		return 1
	}
	if *deprecatedMap {
		fmt.Fprintln(os.Stderr, "note: --map is deprecated; mapping is requested by default.")
	}
	opts := loginOpts{AuthorizeBase: *authorizeBase, APIBase: *apiBase, Mapping: mappingRequested(*noMap), Device: deviceLabel()}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	httpc := defaultHTTPClient()

	usePaste := *paste || !browserLikelyAvailable()
	var tr tokenResponse
	if usePaste {
		tr, err = pasteLogin(ctx, opts, p, httpc, browserOpener, promptStdin)
	} else {
		tr, err = loopbackLogin(ctx, opts, p, httpc, browserOpener)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "login failed:", err)
		return 1
	}

	store, warning, err := newCredentialStore()
	if err != nil {
		fmt.Fprintln(os.Stderr, "login failed:", err)
		return 1
	}
	if warning != "" {
		fmt.Fprintln(os.Stderr, "warning:", warning)
	}
	if err := store.setKey(tr.AccessToken); err != nil {
		fmt.Fprintln(os.Stderr, "failed to store key:", err)
		return 1
	}
	cfg := cliConfig{
		APIBase:       *apiBase,
		AuthorizeBase: *authorizeBase,
		MCPURL:        tr.MCPURL,
		Last4:         tr.Last4,
		ExpiresAt:     tr.ExpiresAt,
		MapperAccess:  tr.MapperAccess,
		ClerkUserID:   tr.ClerkUserID,
	}
	if err := saveConfig(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "failed to save config:", err)
		return 1
	}

	fmt.Printf("\n✓ Logged in. Key …%s, expires %s%s.\n", tr.Last4, tr.ExpiresAt, mapNote(tr.MapperAccess))

	// The credential vault is OFF until the user turns it on. Pairing this
	// machine is what makes it visible in the dashboard and lets a
	// session ask it for a secret, so it is an explicit act, never a side effect
	// of signing in. Signing in should not quietly enroll a laptop as a
	// credential custodian.
	if !vaultEnabled() {
		fmt.Println("\nCredential vault: off. Turn it on with `rindler vault enable`")
		fmt.Println("to store logins on this machine and let your sessions use them.")
	}
	// Say so out loud when mapping was asked for and refused. The server denies it
	// silently (an unentitled workspace just returns false), so without this the
	// only symptom is `rindler map` failing later with a 403 that looks like a bug.
	if !*noMap && !tr.MapperAccess {
		fmt.Println("\nNote: site mapping is NOT enabled on this key — your workspace is not")
		fmt.Println("entitled to it. `rindler map` will be refused until that changes.")
	}

	if !*noMCP {
		fmt.Println("\nInstalling the Rindler MCP into your agents:")
		printAgentResults(os.Stdout, "configured", installAllAgents(mcpEndpoint(cfg), tr.AccessToken))
		fmt.Println("\nRestart Claude Code / Codex to connect.")
	}
	return 0
}

// mcpEndpoint returns the MCP URL to install, preferring the server-returned
// value, then <api_base>/mcp, then the prod default (e.g. when authenticating via
// RINDLER_API_KEY with no saved config).
func mcpEndpoint(cfg cliConfig) string {
	if cfg.MCPURL != "" {
		return cfg.MCPURL
	}
	// Same ladder as every command, so `rindler mcp install` on a box that only
	// has RINDLER_API_KEY and RINDLER_API_BASE set does not quietly install the
	// PRODUCTION endpoint into the agent's config -- a wrong value here is worse
	// than most, because it persists in a file the user will not think to check.
	return resolveAPIBase("", cfg) + "/mcp"
}

// mappingRequested decides whether login asks for site-mapping capability. It is
// its own function so the DEFAULT is pinned by a test: the server always writes
// mapper_access, and the authorization check reads that flag first and
// short-circuits, so defaulting to false hands out keys that are affirmatively
// denied the mapper with no admin fallback. That failure is silent at login and
// only surfaces later as a 403, which is exactly why it must not regress.
func mappingRequested(noMap bool) bool {
	return !noMap
}

func mapNote(mapping bool) string {
	if mapping {
		return " (runtime + mapping)"
	}
	return " (runtime)"
}
