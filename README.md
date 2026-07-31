# `rindler` CLI

[![License: LGPL v3](https://img.shields.io/badge/License-LGPL_v3-blue.svg)](./LICENSE)

Run a task on a website by saying what you want done.

```sh
rindler run chase.com "download last month's statements"
```

`rindler login` signs you in through your Rindler account (OAuth 2.0
Authorization Code + PKCE) and stores a **temporary key bound to that session**
in your OS keyring. Nothing else to configure.

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
| `rindler run <site> "<what you want done>"` | Say it in your own words; Rindler builds it and runs it |
| `rindler login [--paste]` | Sign in |
| `rindler logout` | Sign out on this machine |
| `rindler sites` | The sites you can use |
| `rindler sites add <domain>` | Add a site to your workspace, so everyone on it can use the site |
| `rindler creds add\|list\|show\|rm` | Logins for a site, encrypted on this device (see Credential vault) |
| `rindler usage [--workspace] [--days N] [--json]` | Your automations, the same numbers the dashboard shows |
| `rindler sessions [--json]` | Browsers open on this machine |
| `rindler kill <name>` | Close one |
| `rindler vault status\|enable\|disable` | Turn credential custody on this machine on or off |
| `rindler device status\|list\|serve` | This machine as a paired device, and the relay |
| `rindler status` | Whether you are signed in |
| `rindler whoami` | The signed-in account, plus a `workspace:` line when the key acts in a workspace you do not own |
| `rindler doctor` | Diagnose a broken setup and print the fix |
| `rindler version` | Print the version |

### Running a task

```
$ rindler run chase.com "download last month's statements"
Working on chase.com…
✓ Downloads last month's statements from Chase
  Saved as "Download statements" — run it again any time.
```

Say what you want in your own words. Rindler works out how to do it on that
site, does it once, and saves it so you can run it again. If the site cannot do
what you asked, it says so plainly rather than guessing.

Exit codes, for scripts: `0` it ran, `5` it was saved but the run did not
succeed, `3` the site cannot do that, `4` it needs one thing answered first,
`1` something on our side failed and a retry is reasonable.

`5` and `0` are deliberately different. A task can build perfectly and then
fail on the site (a page that would not load, a login that lapsed). The
automation is saved and worth retrying, but the thing you asked for did not
happen, and a script must not read that as success.

### Login flows

- **Default (loopback):** opens a browser and captures the redirect on
  `http://127.0.0.1:<port>/callback` (literal loopback, RFC 8252). No paste.
- **`--paste`** (automatic on headless/SSH): prints a URL to open in any browser
  on any device; you paste back the `code#state` it shows. The `state` is
  verified (CSRF).

### Two credential lanes

- **Interactive:** `rindler login` stores a session-bound key in the OS keyring
  (`secret-tool` on Linux, `security` on macOS), falling back to a `0600` file
  with a warning when no keyring is present.
- **Headless / CI:** set `RINDLER_API_KEY=rindler_live_…` (a long-lived dashboard
  key). It takes precedence over the stored key and is never written to disk.


### Credential vault

Custody is **off** until you turn it on. Off is a real state: the machine is
unpaired, does not appear under Devices on your dashboard, and no session can
ask it for a login. Signing in does not turn it on.

```sh
rindler vault status      # what this machine is, and what is stored but inert
rindler vault enable      # pair it and start acting as a custodian
rindler vault disable     # unpair it; stored credentials stay on disk
```

### Devices and the relay

```sh
rindler device status     # this machine, and whether the relay can run
rindler device list       # every device on the account, CLI and app
rindler device serve      # hold the socket and answer signed credential requests
```

The relay verifies every request against the server's signing key **before**
opening the vault, refuses one-time-code kinds (a vault holds durable
credentials, never a live code), and seals each secret to the login worker so
Rindler's server relays a ciphertext it cannot read.

The CLI is a **dashboard** tool: it signs in through app.rindler.ai, and the
devices and keys it creates appear there, not in chat.

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

Start with what you can use, then say what you want:

```sh
rindler sites                                          # the sites you can use
rindler run example.com "find the cheapest shoes"      # say it in your own words
```

Rindler keeps two different things apart, on purpose:

- **did it run** — the attempt finished, or it did not
- **what it got back** — what the site actually held, and why it fell short

A run can finish cleanly and still have retrieved nothing usable (a bot wall, an
expired login, a page that changed). `run` exits **non-zero** in that case,
because to a script that is not a success.

Stuck? `rindler doctor` checks the credential, its expiry, the API origin, and
whether the server still accepts your key — then prints the fix rather than the
symptom.
