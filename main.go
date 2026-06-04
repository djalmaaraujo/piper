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
	replayLines   = 100 // recent lines replayed to late-joining viewers
	clientBufSize = 256 // per-client chunk queue; slow clients are dropped
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
	if cfg.help {
		printUsage(os.Stdout)
		os.Exit(0)
	}

	hub := newHub(replayLines)

	ln, err := newListener(cfg.port)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  Error: %v\n", err)
		os.Exit(1)
	}
	srv := &http.Server{Handler: hub.handler()}

	var tunnel Tunnel
	cleanup := func() {
		if tunnel != nil {
			tunnel.Stop()
		}
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cleanup()
		os.Exit(130)
	}()

	go srv.Serve(ln)

	printBanner(cfg, &tunnel)

	var exitCode int
	if cfg.pipe {
		runPipe(hub)
	} else {
		exitCode = runCommand(hub, cfg.command)
	}

	cleanup()
	os.Exit(exitCode)
}

// ---------------------------------------------------------------------------
// args

type config struct {
	port     int
	command  []string
	pipe        bool
	help        bool
	showVersion bool
	public      bool   // expose via a tunnel
	provider    string // forced provider name, or "" for auto-detect
}

func parseArgs(argv []string) (*config, error) {
	c := &config{port: defaultPort}

	if env := os.Getenv("PORT"); env != "" {
		p, err := strconv.Atoi(env)
		if err != nil {
			return nil, fmt.Errorf("invalid PORT env: %q", env)
		}
		c.port = p
	}

	var rest []string
	for i := 0; i < len(argv); i++ {
		switch argv[i] {
		case "-h", "--help":
			c.help = true
		case "--version":
			c.showVersion = true
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
			rest = append(rest, argv[i])
		}
	}

	if c.port < 1 || c.port > 65535 {
		return nil, fmt.Errorf("port out of range: %d", c.port)
	}

	c.command = rest
	if len(rest) == 0 {
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
  piper --public <command>     also expose publicly (auto-detect a tunnel)
  piper --tailscale <command>  force Tailscale Funnel
  piper --cloudflared <cmd>    force a Cloudflare quick tunnel (no account)
  piper --ngrok <command>      force ngrok
  piper --port <n> <command>   listen on a custom port (default 9999)
  <command> | piper            pipe mode (reads stdin)

Viewers:
  curl -N http://localhost:9999     (or open the URL in a browser)
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
}

func newHub(maxLines int) *hub {
	return &hub{clients: make(map[chan []byte]struct{}), maxLines: maxLines}
}

func (h *hub) broadcast(p []byte) {
	chunk := append([]byte(nil), p...)

	h.mu.Lock()
	h.appendReplay(chunk)
	for ch := range h.clients {
		select {
		case ch <- chunk:
		default: // slow consumer: drop rather than block everyone
			close(ch)
			delete(h.clients, ch)
		}
	}
	h.mu.Unlock()

	os.Stdout.Write(p) // echo to our own terminal too
}

// appendReplay keeps the last maxLines complete lines. Caller holds h.mu.
func (h *hub) appendReplay(chunk []byte) {
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
	h.clients[ch] = struct{}{}
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

func (h *hub) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "ok")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
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
	})
	return mux
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
// startup banner

func printBanner(cfg *config, tunnel *Tunnel) {
	localURL := fmt.Sprintf("http://localhost:%d", cfg.port)
	tsIP := tailscaleIP()

	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "  Stream ready!")
	fmt.Fprintf(os.Stderr, "  Local:     %s\n", localURL)
	if tsIP != "" {
		fmt.Fprintf(os.Stderr, "  Tailscale: http://%s:%d\n", tsIP, cfg.port)
	}

	if cfg.public {
		t, url, err := startTunnel(cfg.provider, cfg.port)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  Public:    (failed: %v)\n", err)
		} else {
			*tunnel = t
			fmt.Fprintf(os.Stderr, "  Public:    %s\n", url)
			fmt.Fprintf(os.Stderr, "\n  Viewers:   curl -N %s\n\n", url)
			return
		}
	}

	viewer := localURL
	if tsIP != "" {
		viewer = fmt.Sprintf("http://%s:%d", tsIP, cfg.port)
	}
	fmt.Fprintf(os.Stderr, "\n  Viewers:   curl -N %s\n\n", viewer)
}

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
