<p align="center">
  <img src="assets/logo.svg" alt="Piper" width="520">
</p>

<p align="center">
  <strong>Stream any command's output live over HTTP.</strong><br>
  Watch a long build, a deploy, or a test run from a browser or <code>curl</code> — on your LAN, or the public internet via Tailscale.
</p>

<p align="center">
  <a href="#install"><img src="https://img.shields.io/badge/install-one%20command-22d3ee?style=flat-square" alt="install"></a>
  <img src="https://img.shields.io/badge/node-%3E%3D16-6366f1?style=flat-square" alt="node >= 16">
  <img src="https://img.shields.io/badge/dependencies-zero-38bdf8?style=flat-square" alt="zero dependencies">
  <img src="https://img.shields.io/badge/license-MIT-94a3b8?style=flat-square" alt="MIT license">
</p>

---

## What it does

You run a command. Piper runs it for you, prints the output to your terminal exactly as normal — **and** serves that same output, live, over HTTP. Anyone you share the URL with sees it stream in real time.

```bash
piper npm run build
```

```
  Stream ready!
  Local:     http://localhost:9999
  Tailscale: http://100.x.y.z:9999

  Viewers:   curl -N http://100.x.y.z:9999
```

Now `curl -N http://localhost:9999` from another machine (or open it in a browser) and watch the build scroll by.

## Features

- **Stream live output** — every line of stdout/stderr is broadcast to all connected viewers as it happens.
- **Real TTY, no buffering** — runs your command under `script(1)` so colors and progress bars render and output isn't stuck in a pipe buffer.
- **Zero dependencies** — one ~5 KB Node file. Nothing to `npm install`.
- **Two modes** — run a command directly (`piper <cmd>`) or pipe into it (`somecmd | piper`).
- **Multiple viewers** — connect as many curl/browser clients as you want; each gets the live feed.
- **Replay buffer** — late joiners immediately get the last 100 lines, then keep streaming.
- **LAN sharing out of the box** — auto-detects your Tailscale IP and prints a ready-to-share URL.
- **Public sharing** — `--public` exposes the stream to the internet via Tailscale Funnel.
- **Health check** — `GET /health` returns `ok` for uptime probes.
- **Custom port** — `--port 8080` or `PORT=8080`.

## Install

One command:

```bash
curl -fsSL https://raw.githubusercontent.com/djalmaaraujo/piper/main/install.sh | bash
```

This drops a `piper` executable into `/usr/local/bin` (or `~/.local/bin` if that isn't writable).

<details>
<summary>Other ways to install</summary>

**Run without installing** (needs Node + npm):

```bash
npx github:djalmaaraujo/piper echo hello
```

**Manual:**

```bash
git clone https://github.com/djalmaaraujo/piper.git
cd piper
chmod +x stream.js
ln -s "$PWD/stream.js" /usr/local/bin/piper
```

</details>

**Requirements:** Node.js ≥ 16. For LAN/public sharing, [Tailscale](https://tailscale.com) installed and logged in.

## Usage

```bash
piper <command>                 # run a command and stream its output
piper --public <command>        # also expose publicly via Tailscale Funnel
piper --port 8080 <command>     # listen on a custom port
<command> | piper               # pipe mode (reads stdin)
```

### Examples

```bash
# Watch a build from your phone on the same Tailnet
piper npm run build

# Share a deploy log with a teammate over the internet
piper --public ./deploy.sh

# Stream an existing log file
tail -f /var/log/app.log | piper

# Pick a port
piper --port 8080 pytest -v
```

### Viewing a stream

```bash
curl -N http://localhost:9999      # -N disables curl buffering
```

…or just open the URL in a browser.

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
                  Tailscale Funnel (with --public)
```

Piper launches your command inside a pseudo-terminal via `script(1)` so programs behave as if attached to a real terminal (colors, unbuffered output). Each chunk of output is written to your own stdout, pushed to every connected HTTP client, and appended to a 100-line ring buffer so new viewers get recent context immediately.

## Development

It's a single file — `stream.js` — with no build step and no dependencies.

```bash
git clone https://github.com/djalmaaraujo/piper.git
cd piper
node stream.js echo "hello from piper"
```

Open another terminal and `curl -N http://localhost:9999` to see the stream.

### Contributing

1. Fork the repo and create a branch: `git checkout -b my-change`.
2. Make your change in `stream.js`. Keep it dependency-free.
3. Test both modes manually:
   ```bash
   node stream.js bash -c 'for i in 1 2 3; do echo line $i; sleep 1; done'
   # in another shell:
   curl -N http://localhost:9999
   ```
   And pipe mode:
   ```bash
   printf 'a\nb\nc\n' | node stream.js
   ```
4. Verify `--port`, `--public` (if you have Tailscale), and `/health`.
5. Open a pull request describing what changed and why.

**Style:** match the existing code — plain Node core modules, no transpiler, small and readable.

## Platform notes

- **macOS / Linux** — supported. Piper picks the right `script(1)` invocation per platform.
- **Windows** — not supported (no `script(1)`); use WSL.
- **Public mode** requires Tailscale with Funnel enabled for your tailnet.

## License

[MIT](LICENSE) © Djalma Araújo
