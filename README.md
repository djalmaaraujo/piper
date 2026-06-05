<p align="center">
  <img src="assets/logo.svg" alt="Piper" width="520">
</p>

<p align="center">
  <strong>Stream any command's output live over HTTP.</strong><br>
  Watch a long build, a deploy, or a test run from a browser or <code>curl</code> — locally by default, on your LAN with <code>--lan</code>, or the public internet via Tailscale, Cloudflare, or ngrok.
</p>

<p align="center">
  <a href="#install"><img src="https://img.shields.io/badge/install-one%20command-22d3ee?style=flat-square" alt="install"></a>
  <img src="https://img.shields.io/badge/single-binary-6366f1?style=flat-square" alt="single binary">
  <img src="https://img.shields.io/badge/dependencies-zero-38bdf8?style=flat-square" alt="zero dependencies">
  <img src="https://img.shields.io/badge/license-MIT-94a3b8?style=flat-square" alt="MIT license">
</p>

---

<p align="center">
  <img src="assets/demo.gif" alt="piper screen demo — sharing a window live in the browser" width="900">
</p>

## What it does

You run a command. Piper runs it for you, prints the output to your terminal exactly as normal — **and** serves that same output, live, over HTTP. Anyone you share the URL with sees it stream in real time.

```bash
piper ping google.com
```

```
  Stream ready!  id QGNABDK7  (host)
  Local:     http://localhost:9999/QGNABDK7
  (local only — add --lan to reach this from other devices)

  Terminal:  open http://localhost:9999/QGNABDK7 in a browser
  Viewers:   curl -N http://localhost:9999/QGNABDK7
  Tip:       run with --manager to view multiple pipes at http://localhost:9999/
```

Open `http://localhost:9999/QGNABDK7` in a browser (or `curl -N` it) and watch it stream live, line by line. To reach it from another machine, add `--lan` (LAN / Tailscale) or a tunnel — see [Public sharing](#public-sharing).

<p align="center">
  <img src="assets/screenshot-logs.png" alt="piper log stream in the browser" width="640">
</p>

## Features

- **Single static binary** — written in Go, zero runtime dependencies. No Node, no Python, nothing to install alongside it.
- **Run several at once** — every piper gets a unique id and shares one port. The first to start hosts the server; the rest attach to it automatically (no daemon, no port juggling). `piper --list` shows what's running.
- **Browser view** — open the URL in a browser for a dark, auto-scrolling log page (a tiny dependency-free HTML page embedded in the binary). `curl` still gets the raw stream.
- **Stream live output** — every line of stdout/stderr is broadcast to all connected viewers as it happens.
- **Real TTY, no buffering** — runs your command under `script(1)` so colors and progress bars render and output isn't stuck in a pipe buffer.
- **Two modes** — run a command directly (`piper <cmd>`) or pipe into it (`somecmd | piper`).
- **Multiple viewers** — connect as many curl/browser clients as you want; slow clients are dropped instead of stalling everyone.
- **Replay buffer** — late joiners immediately get the last 100 lines, then keep streaming.
- **Pluggable public sharing** — `--public` auto-detects an installed tunnel; or force one with `--tailscale`, `--cloudflared`, or `--ngrok`.
- **Private by default, LAN with one flag** — streams bind to `localhost` only; add `--lan` to reach them from other devices (and to print your Tailscale IP as a ready-to-share URL).
- **Health check** — `GET /health` returns `ok` for uptime probes.
- **Custom port** — `--port 8080` or `PORT=8080`.

## Install

### Homebrew (macOS & Linux)

```bash
brew install djalmaaraujo/tap/piper
```

### One-command script

```bash
curl -fsSL https://raw.githubusercontent.com/djalmaaraujo/piper/main/install.sh | bash
```

Downloads the right prebuilt binary for your OS/arch into `/usr/local/bin` (or `~/.local/bin`).

### Manual

Grab a binary from the [Releases page](https://github.com/djalmaaraujo/piper/releases), extract, and put `piper` on your `PATH`. Or build from source (see [Development](#development)).

## Usage

```bash
piper <command>                 # run a command and stream its output (localhost only)
piper --lan <command>           # also reach it from other devices on your network
piper --public <command>        # also share publicly (auto-detect a tunnel)
piper --tailscale <command>     # force Tailscale Funnel
piper --cloudflared <command>   # force a Cloudflare quick tunnel (no account)
piper --ngrok <command>         # force ngrok
piper --port 8080 <command>     # share on a custom port
piper --manager <command>       # enable the web index (/) of running streams
piper --list                    # list pipers running on this machine
piper screen "<window name>"    # share a macOS window as a live image
<command> | piper               # pipe mode (reads stdin)
```

piper flags go **before** the command; anything after the command (or after `--`)
is passed to the command. So `piper npm run build --tailscale` runs
`npm run build --tailscale` locally — put `--tailscale` first to tunnel.

### Running several at once

Just run `piper` again — it won't clash. Each instance gets a unique id and is served on the same port:

```
http://localhost:9999/USHS82QK          # browser (terminal-style log page)
curl -N http://localhost:9999/USHS82QK  # raw stream
```

The index of everything running (`http://localhost:9999/`) is **disabled by default** so a viewer can't enumerate your streams — you need the id to reach one. Start any piper with `--manager` to enable the index page.

The first piper to start owns the HTTP server (the "host"); later ones attach to it and push their output over a local unix socket. If the host exits, another instance takes over automatically. The default port is `9999`, but if something else is already using it piper quietly tries the next free port (and other pipers find it there). Running pipers are tracked in `~/piper-config.json`; hitting an id that isn't running returns a friendly *broken pipe* page.

### Screen sharing (macOS)

Share a single window to the same web page, as a live image:

```bash
piper screen                       # native picker — choose a window from a list
piper screen "Chrome"              # share the first window matching "Chrome"
piper screen --list-windows        # print shareable windows
piper screen --fps 10 "Slack"      # target rate (capped to what capture allows)
piper screen --scale 1600 "Cursor" # cap frame width (px); 0 = native
piper screen --quality 40 "Chrome" # JPEG quality 1-100 (lower = smaller/faster)
```

Run with no name and piper pops a **native window picker** (a macOS list dialog) to choose from. It captures the chosen window with `screencapture`, streams it as MJPEG to `/<id>` (image centered on the page), and viewers watch in a browser.

**Permission.** Screen sharing needs macOS **Screen Recording** access (to read window titles and capture). piper asks for it **only the first time you run `piper screen`** — never when streaming commands. If you miss the prompt, enable your terminal under *System Settings → Privacy & Security → Screen Recording* and run it again.

**Resource use / limits.**
- Each frame is one `screencapture` (+ a single `sips` pass for resize + quality). That capture spawns a process, so the real ceiling is ~5–8 fps; piper measures it and reports the actual rate (`asked 30, capture allows ~5`) instead of pretending.
- Bandwidth ≈ **fps × frame size**. Lower `--quality` (default 60) and `--scale` (default 1100) shrink frames a lot — drop them for remote viewers over a tunnel.
- It's a frame stream, not smooth video — great for "watch what's happening", not 60 fps motion.
- **Disk stays bounded**: frames use one temp file, overwritten each tick and deleted right after it's read (and on exit) — nothing accumulates.
- Sharing the window you're *watching* in just produces a harmless infinity-mirror effect; resource use is fixed at the capture rate, no feedback loop.
- **Smoothness:** the page uses a small client-side jitter buffer (default ~400 ms) that plays frames at a steady cadence with catch-up, evening out uneven arrival (e.g. over a tunnel). Override per-view with `?buffer=<ms>` on the URL — `?buffer=0` plays fully live (lowest latency), higher values are smoother but more delayed. It trades latency for smoothness; it can't raise the frame rate.

### Public sharing

`--public` picks the first tunnel it finds installed, in this order: **Tailscale → Cloudflare → ngrok**. Pin a specific one with its flag.

| Provider | Flag | Setup |
|----------|------|-------|
| Tailscale Funnel | `--tailscale` | [Tailscale](https://tailscale.com) installed + logged in, Funnel enabled for your tailnet |
| Cloudflare | `--cloudflared` | [`cloudflared`](https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/downloads/) installed — **quick tunnels need no account** |
| ngrok | `--ngrok` | [`ngrok`](https://ngrok.com) installed + `ngrok config add-authtoken <token>` once |

> **Heads-up:** `--public` exposes the **whole port** — every piper sharing it, not just this one — to the internet, unauthenticated. Don't stream logs that contain secrets.

### Examples

```bash
# Watch a build from your phone on the same LAN / Tailnet
piper --lan npm run build

# Share a deploy log with a teammate, no account needed
piper --cloudflared ./deploy.sh

# Stream an existing log file
tail -f /var/log/app.log | piper

# Pick a port
piper --port 8080 pytest -v
```

### Viewing a stream

Each stream has its own id (shown in the banner). Use it in the URL:

```bash
curl -N http://localhost:9999/QGNABDK7   # -N disables curl buffering
```

…or just open the URL in a browser for the log page.

## How it works

```
              ┌─────────── piper ───────────┐
  your cmd ──►│ script(1) PTY ─► broadcast() │──► your terminal (stdout)
              │                      │        │
              │                      ├────────┼──► HTTP client (curl)
              │                      ├────────┼──► HTTP client (browser)
              │                      └──► ring buffer (last 100 lines)
              └──────────────────────────────┘
                         │
              tunnel provider (with --public):
              Tailscale Funnel · Cloudflare · ngrok
```

Piper launches your command inside a pseudo-terminal via `script(1)` so programs behave as if attached to a real terminal (colors, unbuffered output). Each chunk of output is written to your own stdout, fanned out to every connected HTTP client, and appended to a 100-line ring buffer so new viewers get recent context immediately.

## Platform support

| | Command mode (`piper <cmd>`) | Pipe mode (`cmd \| piper`) | Viewing |
|--|--|--|--|
| **macOS** | ✅ | ✅ | ✅ |
| **Linux** | ✅ | ✅ | ✅ |
| **Windows** | ❌ (needs `script(1)` — use [WSL](https://learn.microsoft.com/windows/wsl/)) | ✅ | ✅ |

Command mode relies on the `script(1)` utility (built in on macOS and Linux). On Windows, pipe a command's output into piper, or run it under WSL.

## Development

Pure Go, standard library only — no third-party modules.

```bash
git clone https://github.com/djalmaaraujo/piper.git
cd piper
go build -o piper .
./piper echo "hello from piper"
```

Open another terminal and `curl -N http://localhost:9999/<id>` (the id is printed in the banner) to see the stream. Plain `/` is the stream index, which is disabled unless you pass `--manager`.

```bash
go vet ./...        # static checks
go test ./...       # unit tests
go build ./...      # compile
```

### Project layout

| File | Purpose |
|------|---------|
| `main.go` | flags, the fan-out hub + replay buffer, command/pipe sources |
| `broker.go` | host/guest roles, port selection, HTTP routing, index, banner |
| `ids.go` | unique stream id generation |
| `state.go` | `~/piper-config.json` + `piper --list` |
| `tunnels.go` | `Tunnel` interface + Tailscale / Cloudflare / ngrok providers |
| `screen_darwin.go` | macOS window capture → MJPEG (`piper screen`) |
| `screen_other.go` | non-macOS stub (screen sharing unsupported) |
| `web.go` | `go:embed` of the browser pages + asset serving |
| `web/index.html` | the embedded browser log page (no third-party JS) |
| `web/screen.html` | the embedded MJPEG viewer page (no third-party JS) |
| `util.go` | TTY detection |
| `.goreleaser.yaml` | release archives, checksums, Homebrew cask |

### Adding a tunnel provider

A tunnel just takes the local HTTP port and returns a public URL by shelling out
to a CLI the user already has installed. Everything lives in `tunnels.go`.

**1. Implement the `Tunnel` interface:**

```go
type Tunnel interface {
	Name() string                   // provider id, e.g. "bore" (matches the --bore flag)
	Available() bool                // is the CLI present (and configured)?
	Start(port int) (string, error) // start tunneling localhost:port, return the public URL
	Stop()                          // kill the process and undo any global state
}
```

Contract for each method:

- **`Name()`** — lowercase id. It's what `--<name>` and the auto-detect loop match on.
- **`Available()`** — cheap, no side effects. Return false if the CLI is missing
  *or* not usable, so auto-detect skips it. Use the `have("<bin>")` helper; check
  config too where it matters (Tailscale, for example, also confirms a Funnel
  hostname resolves before claiming it's available).
- **`Start(port)`** — launch the CLI in the background and return the `https://…`
  URL. Two patterns, both already in the file:
  - **URL printed on stderr/stdout** → use `scanForURL(reader, regexp)`, which
    reads lines in the background and returns the first match on a channel; pair
    it with a `select { case url := <-…: case <-time.After(timeout): }` so you
    never hang (see `cloudflaredTunnel`).
  - **URL from a local API** → poll it until it appears (see `ngrokTunnel`, which
    hits ngrok's `127.0.0.1:4040` API).
  - **URL is deterministic** → just return it (see `tailscaleTunnel`, which
    derives it from the tailnet hostname).
  Keep the started `*exec.Cmd` on the struct so `Stop()` can reach it.
- **`Stop()`** — call `stopProc(cmd)` to kill the process, and reverse anything
  global you set up (e.g. Tailscale runs `tailscale funnel reset`).

**2. Register it for auto-detect** — add it to the slice in `startTunnel`, in
priority order (first `Available()` wins under bare `--public`):

```go
providers := []Tunnel{
	&tailscaleTunnel{},
	&cloudflaredTunnel{},
	&ngrokTunnel{},
	&boreTunnel{}, // ← your provider
}
```

**3. (Optional) add a force flag** so users can pin it. In `parseArgs`
(`main.go`), add your flag to the tunnel case:

```go
case "--tailscale", "--cloudflared", "--ngrok", "--bore":
	c.public = true
	c.provider = strings.TrimPrefix(argv[i], "--")
```

That's it — `startTunnelOnce` starts the tunnel and the banner prints the URL
automatically; no other wiring. Worked sketch for a CLI that prints its URL on
stderr:

```go
type boreTunnel struct{ cmd *exec.Cmd }

func (b *boreTunnel) Name() string    { return "bore" }
func (b *boreTunnel) Available() bool { return have("bore") }

var boreURLRe = regexp.MustCompile(`https://[a-z0-9.-]+\.example\.com`)

func (b *boreTunnel) Start(port int) (string, error) {
	b.cmd = exec.Command("bore", "local", fmt.Sprintf("%d", port), "--to", "bore.example.com")
	stderr, err := b.cmd.StderrPipe()
	if err != nil {
		return "", err
	}
	if err := b.cmd.Start(); err != nil {
		return "", err
	}
	select {
	case url, ok := <-scanForURL(stderr, boreURLRe):
		if !ok || url == "" {
			stopProc(b.cmd)
			return "", fmt.Errorf("bore did not report a URL")
		}
		return url, nil
	case <-time.After(20 * time.Second):
		stopProc(b.cmd)
		return "", fmt.Errorf("timed out waiting for bore URL")
	}
}

func (b *boreTunnel) Stop() { stopProc(b.cmd) }
```

Keep it dependency-free: shell out to the CLI, don't pull in an SDK. Remember a
tunnel exposes the **whole port** to the internet, unauthenticated — same caveat
as the built-in providers.

### Contributing

1. Fork and branch: `git checkout -b my-change`.
2. Keep it dependency-free (standard library only).
3. Test both modes:
   ```bash
   go run . bash -c 'for i in 1 2 3; do echo line $i; sleep 1; done'
   # in another shell (id from the banner):
   curl -N http://localhost:9999/<id>
   ```
4. Run `go vet ./...` and confirm cross-compilation: `GOOS=linux go build -o /dev/null .`
5. Open a pull request describing what changed and why.

### Releasing

Releases are cut by [GoReleaser](https://goreleaser.com) on a tag:

```bash
git tag v0.2.0
git push origin v0.2.0
```

CI builds binaries for macOS/Linux/Windows (amd64 + arm64), publishes a GitHub Release, and updates the Homebrew cask in [`djalmaaraujo/homebrew-tap`](https://github.com/djalmaaraujo/homebrew-tap). The tap update needs a `HOMEBREW_TAP_GITHUB_TOKEN` repo secret (a PAT with `repo` scope on the tap).

## License

[MIT](LICENSE) © Djalma Araújo
