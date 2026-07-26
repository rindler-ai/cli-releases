# `rindler` CLI

[![License: LGPL v3](https://img.shields.io/badge/License-LGPL_v3-blue.svg)](./LICENSE)

Sign in to [Rindler](https://rindler.ai) and install the Rindler MCP into your
coding agents.

`rindler login` authenticates through your Clerk account (OAuth 2.0 Authorization
Code + PKCE), receives a **temporary MCP key bound to that Clerk session**, stores
it in your OS keyring, and wires the Rindler MCP into **Claude Code** and
**Codex**.

## Install

```sh
curl https://rindler.ai/cli | sh
rindler login
```

The install script detects your OS/arch, downloads the matching binary, verifies
its SHA-256 against `SHA256SUMS.txt`, and installs to `/usr/local/bin` (or
`~/.local/bin` when that is not writable).

Prefer to do it by hand? Grab a binary from
[Releases](https://github.com/rindler-ai/cli-releases/releases/latest), check it
against `SHA256SUMS.txt`, `chmod +x`, and put it on your `PATH`.

## Commands

| Command | What it does |
|---|---|
| `rindler login [--paste] [--no-map] [--no-mcp]` | Sign in, mint a session-bound key, install the MCP into Claude Code + Codex |
| `rindler sites` | List the sites you can act on |
| `rindler actions <site>` | Show a site's actions and the inputs each takes |
| `rindler run --site <d> --action <a> [--input k=v]` | Run actions against a site and follow the job |
| `rindler run status <job-id> [--once]` | Follow a run you already started |
| `rindler map <url> [--mode fast\|deep]` | Map a site and follow the run to a verdict |
| `rindler map status <job-id> [--once]` | Follow a run you already started |
| `rindler logout` | Best-effort server-side revoke, then clear local + agent config |
| `rindler status` | Login + MCP-install status |
| `rindler whoami` | The signed-in account |
| `rindler mcp install\|status\|remove` | Manage the MCP install only |
| `rindler doctor` | Diagnose a broken setup and print the fix |
| `rindler version` | Print the version |

### Login flows

- **Default (loopback):** opens a browser and captures the redirect on
  `http://127.0.0.1:<port>/callback` (literal loopback, RFC 8252). No paste.
- **`--paste`** (automatic on headless/SSH): prints a URL to open in any browser
  on any device; you paste back the `code#state` it shows. The `state` is
  verified (CSRF).

`--map` additionally requests site-mapping capability, granted only if your
workspace is entitled to it.

### Two credential lanes

- **Interactive:** `rindler login` stores a session-bound key in the OS keyring
  (`secret-tool` on Linux, `security` on macOS), falling back to a `0600` file
  with a warning when no keyring is present.
- **Headless / CI:** set `RINDLER_API_KEY=rindler_live_…` (a long-lived dashboard
  key). It takes precedence over the stored key and is never written to disk.

## What gets written

- **Claude Code** — `~/.claude.json` (or `$CLAUDE_CONFIG_DIR/.claude.json`):
  `mcpServers.rindler = { type: "http", url, headers: { Authorization: "Bearer …" } }`
- **Codex** — `~/.codex/config.toml` (or `$CODEX_HOME/config.toml`):
  `[mcp_servers.rindler]` with `url` + inline `http_headers`

Both upserts are idempotent and preserve every other server and setting.

## Environment

| Variable | Purpose |
|---|---|
| `RINDLER_API_KEY` | Use this key instead of logging in (CI / headless; never persisted) |
| `RINDLER_CONFIG_DIR` | Override the config dir (default `~/.config/rindler`) |
| `RINDLER_AUTHORIZE_BASE` | Override the consent origin (default `https://app.rindler.ai`) |
| `RINDLER_API_BASE` | Override the API origin (default `https://mcp.rindler.ai`) |

## Build from source

```sh
go build -o rindler .
```

Go 1.26+. No dependencies beyond the Go standard library.

## Why this repo exists

The CLI lives here rather than in Rindler's main repository so that publishing a
release needs **no credential anywhere**:

- The build runs in this repo, so the release is cut with the automatic
  `github.token`. Publishing from a private monorepo into this one would have
  required a long-lived personal access token.
- This repo is public, so `https://rindler.ai/dl` proxies the release assets
  anonymously rather than holding a GitHub token at the public edge.

## License

The rindler CLI is licensed under the GNU Lesser General Public License v3.0
(LGPL-3.0), matching [`rindler-ai/auto-login`](https://github.com/rindler-ai/auto-login).
See [`LICENSE`](./LICENSE); the GPL-3.0 text it incorporates by reference is in
[`COPYING.GPL-3`](./COPYING.GPL-3).

## Doing something with a site

Start from discovery — the names `run` needs are not guessable:

```sh
rindler sites                      # what you can act on
rindler actions example.com        # what that site exposes, and each action's inputs
rindler run --site example.com --action search_products --input query=shoes
```

`run` reports two different things, and keeps them apart on purpose:

- **status** — did the attempt run (`complete`, `failed`, …)
- **retrieval** — what the source actually held, and why it fell short

A job can finish `complete` and still have retrieved nothing usable (a bot wall,
an expired login, a selector that rotted). `run` exits **non-zero** in that case,
because to a script that is not a success.

Stuck? `rindler doctor` checks the credential, its expiry, mapping entitlement,
the API origin, the MCP install, and whether the server still accepts your key —
then prints the fix rather than the symptom.
