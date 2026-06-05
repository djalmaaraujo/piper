// Piper streams a command's (or stdin's) output live over HTTP.
// Viewers connect with `curl -N <url>` or a browser and watch in real time.
// With a tunnel provider (Tailscale / Cloudflare / ngrok) the stream can be
// shared across a tailnet or the public internet.
//
// Zero Go dependencies: standard library only. Command mode shells out to
// script(1) for an unbuffered PTY, exactly like the original snippet.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

const (
	defaultPort   = 9999
	replayLines   = 100     // recent lines replayed to late-joining viewers
	clientBufSize = 256     // per-client chunk queue; slow clients are dropped
	maxPartial    = 1 << 20 // cap on the trailing no-newline replay fragment (1 MiB)
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	cfg, err := parseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "  Error: %v\n", err)
		os.Exit(1)
	}
	if cfg.showVersion {
		fmt.Println("piper", version)
		os.Exit(0)
	}
	if cfg.list {
		runList(cfg.port)
		os.Exit(0)
	}
	if cfg.screen && cfg.listWindows {
		runListWindows()
		os.Exit(0)
	}
	if cfg.help {
		printUsage(os.Stdout)
		os.Exit(0)
	}

	// A local-only stream sits behind loopback, so a short, friendly id is fine.
	// Once the port is exposed (--lan or a public tunnel) the id is the ONLY
	// access control, so make it a long, unguessable bearer token.
	idLen := 8
	if cfg.public || cfg.lan {
		idLen = 24
	}
	id := genID(idLen)
	hub := newHub(replayLines) // local hub: mirrors to our terminal + serves replay
	if cfg.screen {
		// MJPEG frames are binary: don't mirror to the terminal or buffer lines.
		hub.echo = false
		hub.binary = true
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cleanup()
		os.Exit(130)
	}()

	// publish() makes this piper reachable on the shared port — as the host
	// that owns the server, or as a guest that forwards into the existing host.
	// Many pipers share one port; each is addressed by its unique id.
	go publish(cfg, id, hub)

	var exitCode int
	switch {
	case cfg.screen:
		exitCode = runScreenSource(cfg, hub)
	case cfg.pipe:
		runPipe(hub)
	default:
		exitCode = runCommand(hub, cfg.command)
	}

	hub.close()
	cleanup()
	os.Exit(exitCode)
}

// ---------------------------------------------------------------------------
// args

type config struct {
	port        int
	command     []string
	pipe        bool
	help        bool
	showVersion bool
	list        bool   // --list: show running pipers
	manager     bool   // --manager: enable the web index of running streams
	public      bool   // expose via a tunnel
	lan         bool   // --lan: bind all interfaces so LAN/Tailscale peers can reach
	provider    string // forced provider name, or "" for auto-detect

	screen      bool   // `piper screen`: share a window as MJPEG
	screenQuery string // window name to match
	listWindows bool   // `piper screen --list-windows`
	fps         int    // screen capture rate
	scale       int    // screen max width in px (0 = native)
	quality     int    // screen JPEG quality 1-100
}

// kind reports the stream type for the broker/page: "mjpeg" for screen, else "text".
func (c *config) kind() string {
	if c.screen {
		return "mjpeg"
	}
	return "text"
}

func parseArgs(argv []string) (*config, error) {
	c := &config{port: defaultPort, fps: 5, scale: 1100, quality: 60}

	if env := os.Getenv("PORT"); env != "" {
		p, err := strconv.Atoi(env)
		if err != nil {
			return nil, fmt.Errorf("invalid PORT env: %q", env)
		}
		c.port = p
	}

	var rest []string
loop:
	for i := 0; i < len(argv); i++ {
		switch argv[i] {
		case "--":
			// Explicit end of piper's flags; everything after is the command.
			rest = argv[i+1:]
			break loop
		case "-h", "--help":
			c.help = true
		case "--version":
			c.showVersion = true
		case "--list", "--ls":
			c.list = true
		case "--manager":
			c.manager = true
		case "--lan":
			c.lan = true
		case "--list-windows":
			c.listWindows = true
		case "--window":
			if i+1 >= len(argv) {
				return nil, errors.New("--window needs a value")
			}
			c.screenQuery = argv[i+1]
			i++
		case "--fps":
			if i+1 >= len(argv) {
				return nil, errors.New("--fps needs a value")
			}
			n, err := strconv.Atoi(argv[i+1])
			if err != nil {
				return nil, fmt.Errorf("invalid fps: %q", argv[i+1])
			}
			c.fps = n
			i++
		case "--scale":
			if i+1 >= len(argv) {
				return nil, errors.New("--scale needs a value")
			}
			n, err := strconv.Atoi(argv[i+1])
			if err != nil {
				return nil, fmt.Errorf("invalid scale: %q", argv[i+1])
			}
			c.scale = n
			i++
		case "--quality":
			if i+1 >= len(argv) {
				return nil, errors.New("--quality needs a value")
			}
			n, err := strconv.Atoi(argv[i+1])
			if err != nil || n < 1 || n > 100 {
				return nil, fmt.Errorf("invalid quality (1-100): %q", argv[i+1])
			}
			c.quality = n
			i++
		case "--public":
			c.public = true
		case "--tailscale", "--cloudflared", "--ngrok":
			c.public = true
			c.provider = strings.TrimPrefix(argv[i], "--")
		case "--port":
			if i+1 >= len(argv) {
				return nil, errors.New("--port needs a value")
			}
			p, err := strconv.Atoi(argv[i+1])
			if err != nil {
				return nil, fmt.Errorf("invalid port: %q", argv[i+1])
			}
			c.port = p
			i++
		default:
			// First non-flag token starts the command. Stop parsing piper
			// flags here so the command's own arguments (e.g. `mycmd --public`)
			// can never be mistaken for piper flags and silently change behavior.
			rest = argv[i:]
			break loop
		}
	}

	if c.port < 1 || c.port > 65535 {
		return nil, fmt.Errorf("port out of range: %d", c.port)
	}

	// `screen` is a subcommand: the first non-flag word, in any flag order
	// (e.g. `piper --tailscale screen "Chrome"`).
	if len(rest) > 0 && rest[0] == "screen" {
		c.screen = true
		rest = rest[1:]
	}

	c.command = rest
	if c.screen {
		// In screen mode the leftover words are the window name to match.
		if c.screenQuery == "" {
			c.screenQuery = strings.Join(rest, " ")
		}
		return c, nil
	}
	if len(rest) == 0 && !c.list && !c.showVersion {
		// No command => pipe mode, unless stdin is an interactive terminal
		// (nothing piped in), in which case show usage instead of hanging.
		c.pipe = true
		if isTerminal(os.Stdin) {
			c.help = true
		}
	}
	return c, nil
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `piper — stream any command's output live over HTTP

Usage:
  piper <command>              run a command and stream its output
  piper --lan <command>        also reach it from other devices on your network
  piper --public <command>     also expose publicly (auto-detect a tunnel)
  piper --tailscale <command>  force Tailscale Funnel
  piper --cloudflared <cmd>    force a Cloudflare quick tunnel (no account)
  piper --ngrok <command>      force ngrok
  piper --port <n> <command>   share on a custom port (default 9999)
  piper --manager <command>    enable the web index (/) of running streams
  piper --list                 list pipers currently running on this machine
  piper screen "<window>"      share a macOS window as a live image (MJPEG)
  piper screen --list-windows  list shareable windows
  <command> | piper            pipe mode (reads stdin)

piper flags must come BEFORE the command; anything after the command (or
after --) belongs to the command. By default the stream is bound to
localhost only — use --lan or --public to expose it on the network.

Screen options:  --fps <n> (default 5)   --scale <px width> (0 = native)

Many pipers share one port; each gets a unique id and its own URL:
  http://localhost:9999/<id>          (browser)
  curl -N http://localhost:9999/<id>  (terminal)
  http://localhost:9999/              (index of running pipers)
`)
}

// ---------------------------------------------------------------------------
// hub: fan-out broadcaster with a recent-lines replay buffer

type hub struct {
	mu       sync.Mutex
	clients  map[chan []byte]struct{}
	lines    [][]byte // completed recent lines (each ends with '\n')
	partial  []byte   // trailing bytes with no newline yet
	maxLines int
	echo     bool // mirror output to this process's stdout
	binary   bool // MJPEG/binary stream: no line replay, no stdout echo
	closed   bool
}

func newHub(maxLines int) *hub {
	return &hub{clients: make(map[chan []byte]struct{}), maxLines: maxLines, echo: true}
}

func (h *hub) broadcast(p []byte) {
	chunk := append([]byte(nil), p...)

	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.appendReplay(chunk)
	for ch := range h.clients {
		select {
		case ch <- chunk:
		default: // slow consumer: drop rather than block everyone
			close(ch)
			delete(h.clients, ch)
		}
	}
	echo := h.echo
	h.mu.Unlock()

	if echo {
		os.Stdout.Write(p) // mirror to our own terminal
	}
}

func (h *hub) isClosed() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.closed
}

// close ends all client streams and stops accepting further output.
func (h *hub) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	for ch := range h.clients {
		close(ch)
		delete(h.clients, ch)
	}
}

// appendReplay keeps the last maxLines complete lines. Caller holds h.mu.
func (h *hub) appendReplay(chunk []byte) {
	if h.binary {
		return // binary streams (MJPEG) aren't line-buffered for replay
	}
	data := append(h.partial, chunk...)
	for {
		i := strings.IndexByte(string(data), '\n')
		if i < 0 {
			break
		}
		line := append([]byte(nil), data[:i+1]...)
		h.lines = append(h.lines, line)
		data = data[i+1:]
	}
	// Bound the trailing no-newline fragment so a source that emits a huge line
	// without a '\n' can't grow this buffer (and every new viewer's snapshot)
	// without limit. Keep only the most recent maxPartial bytes.
	if len(data) > maxPartial {
		data = data[len(data)-maxPartial:]
	}
	h.partial = append([]byte(nil), data...)
	if len(h.lines) > h.maxLines {
		h.lines = h.lines[len(h.lines)-h.maxLines:]
	}
}

// register adds a client and atomically returns the current replay snapshot,
// so no chunk is lost between snapshot and subscription.
func (h *hub) register() (chan []byte, []byte) {
	ch := make(chan []byte, clientBufSize)
	h.mu.Lock()
	var snap []byte
	for _, l := range h.lines {
		snap = append(snap, l...)
	}
	snap = append(snap, h.partial...)
	if h.closed {
		close(ch) // source already ended; reader will exit after the snapshot
	} else {
		h.clients[ch] = struct{}{}
	}
	h.mu.Unlock()
	return ch, snap
}

func (h *hub) unregister(ch chan []byte) {
	h.mu.Lock()
	if _, ok := h.clients[ch]; ok {
		delete(h.clients, ch)
		close(ch)
	}
	h.mu.Unlock()
}

// streamHandler streams the replay buffer followed by live output as
// chunked text/plain, until the client disconnects or the source ends.
func (h *hub) streamHandler(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	if h.binary {
		w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary="+mjpegBoundary)
	} else {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	}
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	ch, snap := h.register()
	defer h.unregister(ch)

	if len(snap) > 0 {
		w.Write(snap)
		flusher.Flush()
	}

	for {
		select {
		case <-r.Context().Done():
			return
		case chunk, open := <-ch:
			if !open {
				return
			}
			if _, err := w.Write(chunk); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// ---------------------------------------------------------------------------
// sources

func runPipe(h *hub) {
	buf := make([]byte, 32*1024)
	for {
		n, err := os.Stdin.Read(buf)
		if n > 0 {
			h.broadcast(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

// runCommand runs the command under script(1) for an unbuffered PTY, so colors
// and progress bars render. macOS (BSD script) takes argv directly; Linux
// (util-linux script) takes a single -c command string, so we shell-quote.
func runCommand(h *hub, command []string) int {
	cmd, err := scriptCommand(command)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  Error: %v\n", err)
		return 1
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "  Error: %v\n", err)
		return 1
	}
	cmd.Stderr = cmd.Stdout // script merges via the pty; keep stderr with it

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "  Error: could not start command: %v\n", err)
		return 1
	}

	buf := make([]byte, 32*1024)
	for {
		n, rerr := stdout.Read(buf)
		if n > 0 {
			h.broadcast(buf[:n])
		}
		if rerr != nil {
			break
		}
	}

	code := exitCodeOf(cmd.Wait())
	fmt.Fprintf(os.Stderr, "\n  Command exited (code %d)\n", code)
	return code
}

// scriptCommand builds the script(1) invocation for the current platform.
func scriptCommand(command []string) (*exec.Cmd, error) {
	if len(command) == 0 {
		return nil, errors.New("no command given")
	}
	switch runtime.GOOS {
	case "darwin":
		// BSD: script -q /dev/null cmd arg1 arg2...  (argv passthrough, no quoting)
		args := append([]string{"-q", "/dev/null"}, command...)
		return exec.Command("script", args...), nil
	case "linux":
		// util-linux: script -e -q -c "<cmd string>" /dev/null
		// -e returns the child's exit code; -c takes one shell string.
		cmdStr := shellJoin(command)
		return exec.Command("script", "-e", "-q", "-c", cmdStr, "/dev/null"), nil
	default:
		return nil, fmt.Errorf("command mode needs script(1), unavailable on %s; use pipe mode (cmd | piper) or WSL", runtime.GOOS)
	}
}

// shellJoin POSIX-quotes each arg so spaces/quotes survive a shell round-trip.
func shellJoin(args []string) string {
	parts := make([]string, len(args))
	for i, a := range args {
		parts[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(parts, " ")
}

func exitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return 1
}

// ---------------------------------------------------------------------------
// tailscale helpers (used by the banner and the tunnel providers)

func tailscaleIP() string {
	out, err := exec.Command("tailscale", "ip", "-4").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.Split(string(out), "\n")[0])
}

func tailscaleFunnelURL() string {
	out, err := exec.Command("tailscale", "status", "--json").Output()
	if err != nil {
		return ""
	}
	var status struct {
		Self struct {
			DNSName string `json:"DNSName"`
		} `json:"Self"`
	}
	if json.Unmarshal(out, &status) != nil {
		return ""
	}
	host := strings.TrimSuffix(status.Self.DNSName, ".")
	if host == "" {
		return ""
	}
	return "https://" + host
}
