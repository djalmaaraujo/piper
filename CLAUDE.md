# piper — working conventions

piper streams a command's (or stdin's) output live over HTTP. Single static Go
binary. Keep it small, readable, and boring.

## Zero dependencies (hard rule)

- **No Go module dependencies.** `go.mod` must have an empty `require` — standard
  library only. If you reach for a third-party package, find a stdlib way instead.
- **No third-party JavaScript** in the browser UI. The page is one self-contained
  `web/index.html` (a plain `fetch` loop into a `<pre>`), embedded with `go:embed`.
  We deliberately removed xterm.js to avoid the supply-chain surface.
- The only acceptable external dependencies are **optional runtime CLIs** the user
  already has: `script(1)` (PTY), `tailscale`, `cloudflared`, `ngrok`. They're
  optional and shelled out to — never bundled.

## Design principles

- **Mega simple.** Prefer the obvious solution. Don't add a daemon, a database, a
  config DSL, or a framework. If a feature needs one of those, push back first.
- **Single binary, cross-platform.** Must `go build` for darwin/linux/windows ×
  amd64/arm64. Command mode needs `script(1)` (macOS/Linux); Windows is pipe-mode
  / WSL only — keep it that way unless asked.
- **Match the existing code.** Plain stdlib, no clever abstractions, comments only
  where intent isn't obvious.

## Architecture (multi-instance)

- Many pipers share one port. First to bind the TCP port is the **host** (runs the
  HTTP server, routes by stream id); later ones are **guests** that push output to
  the host over a **local unix socket** (`~/.piper-<port>.sock`, 0600). No daemon —
  the host is just the first piper's own process. If the host exits, a guest
  re-binds and promotes itself.
- Each piper gets a unique id; URLs: `/<id>` (browser), `/<id>/stream` (curl),
  `/<id>/info` (JSON), `/` (index of running pipers). Unknown id → friendly
  *broken pipe* page.
- If the default port (9999) is taken by a non-piper, scan upward for a free one;
  every piper scans the same sequence so they converge on the same port.
- Running pipers are tracked in `~/piper-config.json` (0600); `piper --list` reads it.

## Security

- **Loopback by default.** The TCP server binds `127.0.0.1` — reachable only
  from this machine. `--lan` (or `--public`, which needs it) widens the bind to
  `0.0.0.0`. `bindHost(cfg)` is the single source of truth.
- **piper flags must precede the command.** `parseArgs` stops consuming piper
  flags at the first non-flag token (and at `--`), so a command's own args
  (`mycmd --public`) can never silently flip on a tunnel.
- The TCP port is **read-only**: non-GET/HEAD requests get 405. There is **no
  network write surface** — output is fed only by a same-user local process over
  the unix socket. The HTTP server sets `ReadHeaderTimeout`/`IdleTimeout`/
  `MaxHeaderBytes` to blunt Slowloris-style DoS.
- **Stream id is the only access control.** Local ids are 8 chars (friendly);
  exposed ids (`--lan`/`--public`) are 24 chars (~118-bit bearer token), drawn
  with rejection sampling so every char is uniform. `/info` deliberately omits
  the command line (args may carry secrets).
- The web index (`/`) is an enumeration surface — **off by default**, returns
  404. `--manager` opts in, and the index lists **only** streams that themselves
  set `--manager`, never co-located ones that didn't.
- State and socket files are `0600`; the socket lives in a `0700` `~/.piper/`
  dir. The host refuses to serve if it can't lock the socket down.
- `--public` (tunnels) exposes the *whole* port to the internet, unauthenticated
  — every piper sharing it, not just the one you ran. Don't stream secrets; the
  banner warns at startup.

## Browser page

- Dark slate theme (`#0f172a` bg, `#e6e8f0` text), hidden-but-scrollable, padded,
  sticky-bottom autoscroll, DOM node cap (~4000) to bound memory.
- Strip ANSI/control sequences client-side; render plain text via `textContent`
  (never `innerHTML`).
- Discreet icon-only brand bottom-right linking to the repo (embedded base64).

## Workflow

- **Never publish without explicit approval.** Work on a branch. "Publish" = merge
  to `main` + a `vX.Y.Z` tag (GoReleaser builds binaries + updates the Homebrew
  cask in `djalmaaraujo/homebrew-tap`). Pushing a feature branch is fine.
- Before shipping: `go vet ./...`, `go test ./...`, and cross-compile all five
  targets. Add/adjust tests for new behavior.
- Releases need the `HOMEBREW_TAP_GITHUB_TOKEN` repo secret for CI (PAT with `repo`
  scope on the tap). Local releases: `GITHUB_TOKEN=$(gh auth token)
  HOMEBREW_TAP_GITHUB_TOKEN=$(gh auth token) goreleaser release --clean`.
