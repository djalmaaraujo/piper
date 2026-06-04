package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Many pipers share one TCP port. The first to bind it becomes the host and
// runs the HTTP server, routing by stream id. Later pipers become guests: they
// forward their output to the host over a local UNIX SOCKET — never the
// network. So the TCP port has no write surface at all (a POST gets 405); the
// only way to feed a stream is a same-user process on this machine. If the host
// exits, the port frees and a guest promotes itself on its next loop iteration.
//
// No background daemon: the "host" is simply the first piper's own process.

type stream struct {
	id      string
	cmd     string
	pid     int
	role    string // "host" or "guest"
	started string
	manager bool // this piper opted into exposing the web index
	hub     *hub
}

type registry struct {
	mu      sync.Mutex
	port    int
	streams map[string]*stream
	order   []string
}

func newRegistry(port int) *registry {
	return &registry{port: port, streams: map[string]*stream{}}
}

func (rg *registry) add(s *stream) {
	rg.mu.Lock()
	if _, ok := rg.streams[s.id]; !ok {
		rg.order = append(rg.order, s.id)
	}
	rg.streams[s.id] = s
	rg.persistLocked()
	rg.mu.Unlock()
}

func (rg *registry) remove(id string) {
	rg.mu.Lock()
	if _, ok := rg.streams[id]; ok {
		delete(rg.streams, id)
		for i, v := range rg.order {
			if v == id {
				rg.order = append(rg.order[:i], rg.order[i+1:]...)
				break
			}
		}
	}
	rg.persistLocked()
	rg.mu.Unlock()
}

func (rg *registry) get(id string) (*stream, bool) {
	rg.mu.Lock()
	defer rg.mu.Unlock()
	s, ok := rg.streams[id]
	return s, ok
}

// managerOn reports whether any running piper enabled the web index.
func (rg *registry) managerOn() bool {
	rg.mu.Lock()
	defer rg.mu.Unlock()
	for _, s := range rg.streams {
		if s.manager {
			return true
		}
	}
	return false
}

func (rg *registry) list() []*stream {
	rg.mu.Lock()
	defer rg.mu.Unlock()
	out := make([]*stream, 0, len(rg.order))
	for _, id := range rg.order {
		if s, ok := rg.streams[id]; ok {
			out = append(out, s)
		}
	}
	return out
}

// persistLocked writes the current registry to ~/piper-config.json. Holds mu.
func (rg *registry) persistLocked() {
	s := stateFile{Port: rg.port}
	for _, id := range rg.order {
		if st := rg.streams[id]; st != nil {
			s.Pipers = append(s.Pipers, piperEntry{
				ID: st.id, Cmd: st.cmd, PID: st.pid, Started: st.started, Role: st.role,
			})
		}
	}
	_ = writeState(s)
}

// ---------------------------------------------------------------------------
// HTTP routing (host only)

func (rg *registry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Read-only by construction: no endpoint ever ingests data over the network.
	// Reject writes explicitly so the intent is unmistakable.
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "piper serves reads only", http.StatusMethodNotAllowed)
		return
	}

	if r.URL.Path == "/health" {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "ok")
		return
	}

	path := strings.Trim(r.URL.Path, "/")
	if path == "" {
		// The index lists every running stream, so it's an enumeration surface.
		// Off by default — only served when a piper opted in with --manager.
		if rg.managerOn() {
			rg.serveIndex(w, r)
		} else {
			rg.serveIndexDisabled(w, r)
		}
		return
	}

	parts := strings.SplitN(path, "/", 2)
	id := parts[0]
	st, ok := rg.get(id)
	if !ok || !validID(id) {
		rg.serveBrokenPipe(w, r, id)
		return
	}
	switch {
	case len(parts) == 2 && parts[1] == "stream":
		st.hub.streamHandler(w, r)
	case len(parts) == 2 && parts[1] == "info":
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": st.id, "started": st.started, "cmd": st.cmd, "role": st.role,
		})
	case len(parts) == 1:
		if wantsHTML(r) {
			serveAsset("web/index.html", "text/html; charset=utf-8")(w, r)
		} else {
			st.hub.streamHandler(w, r)
		}
	default:
		rg.serveBrokenPipe(w, r, id)
	}
}

// guestHeader is the first line a guest sends over the unix socket, before its
// raw output stream.
type guestHeader struct {
	ID      string `json:"id"`
	Cmd     string `json:"cmd"`
	PID     int    `json:"pid"`
	Started string `json:"started"`
	Manager bool   `json:"manager"`
}

// acceptGuests serves the host's unix socket: each connecting guest registers a
// stream (header line) then streams its output.
func acceptGuests(ul net.Listener, rg *registry) {
	for {
		conn, err := ul.Accept()
		if err != nil {
			return
		}
		go handleGuestConn(conn, rg)
	}
}

func handleGuestConn(conn net.Conn, rg *registry) {
	defer conn.Close()
	br := bufio.NewReader(conn)
	line, err := br.ReadString('\n')
	if err != nil {
		return
	}
	var hdr guestHeader
	if json.Unmarshal([]byte(line), &hdr) != nil || !validID(hdr.ID) {
		return
	}

	hb := newHub(replayLines)
	hb.echo = false // don't print a guest's output on the host's terminal
	st := &stream{
		id: hdr.ID, cmd: hdr.Cmd, pid: hdr.PID, role: "guest",
		started: hdr.Started, manager: hdr.Manager, hub: hb,
	}
	rg.add(st)
	defer rg.remove(hdr.ID)

	buf := make([]byte, 32*1024)
	for {
		n, err := br.Read(buf)
		if n > 0 {
			hb.broadcast(buf[:n])
		}
		if err != nil {
			break
		}
	}
	hb.close()
}

func (rg *registry) serveIndex(w http.ResponseWriter, r *http.Request) {
	streams := rg.list()
	if !wantsHTML(r) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if len(streams) == 0 {
			io.WriteString(w, "no pipers running\n")
			return
		}
		for _, s := range streams {
			fmt.Fprintf(w, "%-8s  %-6s  %s\n", s.id, s.role, s.cmd)
		}
		return
	}

	var rows strings.Builder
	for _, s := range streams {
		rows.WriteString(fmt.Sprintf(
			`<li><a href="/%s">%s</a> <span class="role">%s</span> <span class="cmd">%s</span></li>`,
			s.id, s.id, s.role, html.EscapeString(s.cmd)))
	}
	if len(streams) == 0 {
		rows.WriteString(`<li class="empty">nothing streaming yet</li>`)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, indexHTML, rows.String())
}

func (rg *registry) serveIndexDisabled(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNotFound)
	if !wantsHTML(r) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, "index disabled — start a piper with --manager to enable it.\nopen a stream directly at /<id>.\n")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, indexDisabledHTML)
}

func (rg *registry) serveBrokenPipe(w http.ResponseWriter, r *http.Request, id string) {
	w.WriteHeader(http.StatusNotFound)
	if !wantsHTML(r) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "💥 broken pipe — no stream %q is flowing here.\nSee what's running:  curl -N http://localhost:%d/\n", id, rg.port)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, brokenPipeHTML, html.EscapeString(id))
}

// ---------------------------------------------------------------------------
// roles

// publish keeps this piper reachable on a shared port for its whole life.
func publish(cfg *config, id string, h *hub) {
	for {
		if h.isClosed() {
			return // our source already ended; nothing to publish
		}
		port, ln := claimPort(cfg.port)
		if ln != nil {
			becomeHost(ln, cfg, port, id, h) // blocks until the process exits
			return
		}
		if gerr := becomeGuest(cfg, port, id, h); gerr == nil {
			return // our stream ended cleanly
		}
		if h.isClosed() {
			return
		}
		time.Sleep(300 * time.Millisecond) // host vanished; loop may promote us
	}
}

// claimPort scans upward from start to find the port piper should use.
//   - bindable           => we become the host (returns the listener)
//   - held by a piper     => we join it as a guest (returns nil listener)
//   - held by something else => skipped, so we stay out of its way
//
// Every piper scans the same sequence, so they all converge on the same port
// even when the default is taken by an unrelated process.
func claimPort(start int) (int, net.Listener) {
	const span = 64
	for p := start; p < start+span; p++ {
		if p < 1 || p > 65535 {
			break
		}
		ln, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", p))
		if err == nil {
			return p, ln // free — host here
		}
		if piperHostAt(p) {
			return p, nil // a piper already owns this port — join as guest
		}
		// occupied by a non-piper process: try the next port
	}
	return start, nil // give up scanning; try to attach at the default
}

// piperHostAt reports whether a piper host is listening on port p, by probing
// its local control socket (network processes can't fake this).
func piperHostAt(p int) bool {
	c, err := net.DialTimeout("unix", socketPath(p), 200*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// socketPath is the host's local control socket for this port. It lives in the
// user's home dir with 0600 perms, so only the same user can connect — and it's
// not reachable over the network at all.
func socketPath(port int) string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.TempDir()
	}
	return filepath.Join(home, fmt.Sprintf(".piper-%d.sock", port))
}

func becomeHost(ln net.Listener, cfg *config, port int, id string, h *hub) {
	rg := newRegistry(port)
	rg.add(&stream{
		id: id, cmd: cmdLabel(cfg), pid: os.Getpid(), role: "host",
		started: nowStamp(), manager: cfg.manager, hub: h,
	})

	// Local unix socket for guests. Winning the TCP bind makes us the authority,
	// so it's safe to clear a stale socket file left by a previous host.
	sp := socketPath(port)
	os.Remove(sp)
	if ul, err := net.Listen("unix", sp); err == nil {
		os.Chmod(sp, 0o600)
		hostMu.Lock()
		hostSock = sp
		hostMu.Unlock()
		go acceptGuests(ul, rg)
		defer func() { ul.Close(); os.Remove(sp) }()
	}

	tunnelURL := startTunnelOnce(cfg, port)
	printBanner(port, id, "host", cfg.manager, tunnelURL)
	srv := &http.Server{Handler: rg}
	_ = srv.Serve(ln)
}

func becomeGuest(cfg *config, port int, id string, h *hub) error {
	conn, err := net.Dial("unix", socketPath(port))
	if err != nil {
		return err // host not reachable (maybe just died) — caller may promote us
	}
	defer conn.Close()

	hdr, _ := json.Marshal(guestHeader{
		ID: id, Cmd: cmdLabel(cfg), PID: os.Getpid(), Started: nowStamp(), Manager: cfg.manager,
	})
	if _, err := conn.Write(append(hdr, '\n')); err != nil {
		return err
	}

	tunnelURL := startTunnelOnce(cfg, port)
	printBanner(port, id, "guest", cfg.manager, tunnelURL)

	ch, snap := h.register()
	defer h.unregister(ch)
	if len(snap) > 0 {
		if _, err := conn.Write(snap); err != nil {
			return err
		}
	}
	for {
		chunk, open := <-ch
		if !open {
			return nil // our source ended cleanly
		}
		if _, err := conn.Write(chunk); err != nil {
			return err // host died — caller may promote us
		}
	}
}

func cmdLabel(cfg *config) string {
	if cfg.pipe {
		return "(pipe)"
	}
	return strings.Join(cfg.command, " ")
}

// ---------------------------------------------------------------------------
// tunnel (process-global; started by whichever role runs first)

var (
	tunnelMu        sync.Mutex
	activeTunnel    Tunnel
	activeTunnelURL string

	hostMu   sync.Mutex
	hostSock string // unix socket path we created as host, if any
)

func startTunnelOnce(cfg *config, port int) string {
	if !cfg.public {
		return ""
	}
	tunnelMu.Lock()
	defer tunnelMu.Unlock()
	if activeTunnel != nil {
		return activeTunnelURL
	}
	t, u, err := startTunnel(cfg.provider, port)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  Public:    (failed: %v)\n", err)
		return ""
	}
	activeTunnel, activeTunnelURL = t, u
	return u
}

func stopTunnel() {
	tunnelMu.Lock()
	defer tunnelMu.Unlock()
	if activeTunnel != nil {
		activeTunnel.Stop()
		activeTunnel = nil
	}
}

// cleanup tears down anything this process started before it exits.
// (os.Exit skips defers, so the host socket is removed here too.)
func cleanup() {
	stopTunnel()
	hostMu.Lock()
	sp := hostSock
	hostMu.Unlock()
	if sp != "" {
		os.Remove(sp)
		// We owned the state file as host; clear it so `--list` isn't stale.
		// A surviving guest that promotes will rewrite it within a moment.
		_ = writeState(stateFile{})
	}
}

// ---------------------------------------------------------------------------
// banner

func printBanner(port int, id, role string, manager bool, tunnelURL string) {
	base := fmt.Sprintf("http://localhost:%d", port)
	tsIP := tailscaleIP()
	w := os.Stderr

	fmt.Fprintln(w)
	fmt.Fprintf(w, "  Stream ready!  id %s  (%s)\n", id, role)
	fmt.Fprintf(w, "  Local:     %s/%s\n", base, id)
	if tsIP != "" {
		fmt.Fprintf(w, "  Tailscale: http://%s:%d/%s\n", tsIP, port, id)
	}
	if tunnelURL != "" {
		fmt.Fprintf(w, "  Public:    %s/%s\n", tunnelURL, id)
	}
	// pick the most shareable root for the viewer URLs
	root := base
	switch {
	case tunnelURL != "":
		root = tunnelURL
	case tsIP != "":
		root = fmt.Sprintf("http://%s:%d", tsIP, port)
	}
	viewer := root + "/" + id

	fmt.Fprintf(w, "\n  Terminal:  open %s in a browser\n", viewer)
	fmt.Fprintf(w, "  Viewers:   curl -N %s\n", viewer)
	if manager {
		fmt.Fprintf(w, "  Index:     %s/\n", root)
	}
	fmt.Fprintln(w)
	if !manager {
		fmt.Fprintf(w, "  Tip:       run with --manager to view multiple pipes at %s/\n\n", root)
	}
}

// ---------------------------------------------------------------------------
// inline pages

const indexDisabledHTML = `<!doctype html><html lang="en"><head>
<meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>piper</title>
<style>
  html,body { height:100%; margin:0; background:#0f172a; color:#e6e8f0;
              font:14px/1.6 ui-monospace,SFMono-Regular,Menlo,monospace; }
  .wrap { height:100%; display:flex; flex-direction:column; align-items:center; justify-content:center; text-align:center; padding:24px; }
  h1 { font-size:20px; margin:0 0 8px; }
  p { color:#94a3b8; margin:2px 0; }
  code { color:#e6e8f0; }
</style></head><body><div class="wrap">
<h1>🔒 index disabled</h1>
<p>listing every stream is off by default.</p>
<p>start a piper with <code>--manager</code> to enable this page,</p>
<p>or open a stream directly at <code>/&lt;id&gt;</code>.</p>
</div></body></html>`

const indexHTML = `<!doctype html><html lang="en"><head>
<meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>piper</title>
<style>
  body { margin:0; min-height:100%%; background:#0f172a; color:#e6e8f0;
         font:14px/1.6 ui-monospace,SFMono-Regular,Menlo,monospace; padding:32px; box-sizing:border-box; }
  h1 { font-size:18px; font-weight:700; margin:0 0 4px; }
  p.sub { color:#64748b; margin:0 0 24px; }
  ul { list-style:none; padding:0; margin:0; }
  li { padding:10px 12px; border:1px solid #1e293b; border-radius:8px; margin-bottom:8px; }
  li a { color:#22d3ee; text-decoration:none; font-weight:700; }
  li a:hover { text-decoration:underline; }
  .role { color:#6366f1; margin-left:8px; }
  .cmd { color:#94a3b8; margin-left:8px; }
  .empty { color:#64748b; border-style:dashed; }
  #brand { position:fixed; bottom:12px; right:12px; opacity:.4; transition:opacity .15s; }
  #brand:hover { opacity:.9; }
  #brand img { height:48px; display:block; }
</style></head><body>
<h1>piper</h1><p class="sub">streams running on this machine</p>
<ul>%s</ul>
<a id="brand" href="https://github.com/djalmaaraujo/piper" target="_blank" rel="noopener" title="piper on GitHub"><img alt="piper" src="data:image/svg+xml;base64,PHN2ZyB3aWR0aD0iMTgwIiBoZWlnaHQ9IjE4MCIgdmlld0JveD0iMTAgMjAgMTgwIDE4MCIgZmlsbD0ibm9uZSIgeG1sbnM9Imh0dHA6Ly93d3cudzMub3JnLzIwMDAvc3ZnIiByb2xlPSJpbWciIGFyaWEtbGFiZWw9IlBpcGVyIGljb24iPgogIDxkZWZzPgogICAgPGxpbmVhckdyYWRpZW50IGlkPSJwaXBlR3JhZCIgeDE9IjAiIHkxPSIwIiB4Mj0iMSIgeTI9IjEiPgogICAgICA8c3RvcCBvZmZzZXQ9IjAlIiBzdG9wLWNvbG9yPSIjMjJkM2VlIi8+CiAgICAgIDxzdG9wIG9mZnNldD0iMTAwJSIgc3RvcC1jb2xvcj0iIzYzNjZmMSIvPgogICAgPC9saW5lYXJHcmFkaWVudD4KICAgIDxsaW5lYXJHcmFkaWVudCBpZD0iYmdHcmFkIiB4MT0iMCIgeTE9IjAiIHgyPSIxIiB5Mj0iMSI+CiAgICAgIDxzdG9wIG9mZnNldD0iMCUiIHN0b3AtY29sb3I9IiMwZjE3MmEiLz4KICAgICAgPHN0b3Agb2Zmc2V0PSIxMDAlIiBzdG9wLWNvbG9yPSIjMWUxYjRiIi8+CiAgICA8L2xpbmVhckdyYWRpZW50PgogIDwvZGVmcz4KCiAgPHJlY3QgeD0iMjAiIHk9IjMwIiB3aWR0aD0iMTYwIiBoZWlnaHQ9IjE2MCIgcng9IjI4IiBmaWxsPSJ1cmwoI2JnR3JhZCkiIHN0cm9rZT0idXJsKCNwaXBlR3JhZCkiIHN0cm9rZS13aWR0aD0iNCIvPgoKICA8IS0tIHByb21wdCBjaGV2cm9uIC0tPgogIDxwYXRoIGQ9Ik01OCA4NiBMODYgMTEwIEw1OCAxMzQiIHN0cm9rZT0idXJsKCNwaXBlR3JhZCkiIHN0cm9rZS13aWR0aD0iMTEiIHN0cm9rZS1saW5lY2FwPSJyb3VuZCIgc3Ryb2tlLWxpbmVqb2luPSJyb3VuZCIgZmlsbD0ibm9uZSIvPgoKICA8IS0tIHN0cmVhbWluZyBwaXBlOiB2ZXJ0aWNhbCBiYXIgd2l0aCBmbG93aW5nIGRvdHMgLS0+CiAgPHJlY3QgeD0iMTA0IiB5PSI4NCIgd2lkdGg9IjEyIiBoZWlnaHQ9IjUyIiByeD0iNiIgZmlsbD0idXJsKCNwaXBlR3JhZCkiLz4KICA8Y2lyY2xlIGN4PSIxMzIiIGN5PSIxMTAiIHI9IjciIGZpbGw9IiMyMmQzZWUiLz4KICA8Y2lyY2xlIGN4PSIxNTAiIGN5PSIxMTAiIHI9IjUiIGZpbGw9IiMzOGJkZjgiIG9wYWNpdHk9IjAuNyIvPgogIDxjaXJjbGUgY3g9IjE2NCIgY3k9IjExMCIgcj0iMy41IiBmaWxsPSIjNjM2NmYxIiBvcGFjaXR5PSIwLjUiLz4KPC9zdmc+Cg=="></a>
</body></html>`

const brokenPipeHTML = `<!doctype html><html lang="en"><head>
<meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>broken pipe</title>
<style>
  html,body { height:100%%; margin:0; background:#0f172a; color:#e6e8f0;
              font:14px/1.6 ui-monospace,SFMono-Regular,Menlo,monospace; }
  .wrap { height:100%%; display:flex; flex-direction:column; align-items:center; justify-content:center; text-align:center; padding:24px; }
  pre { color:#22d3ee; margin:0 0 16px; font-size:13px; }
  h1 { font-size:20px; margin:0 0 8px; }
  p { color:#94a3b8; margin:2px 0; }
  code { color:#e6e8f0; }
  a { color:#22d3ee; margin-top:18px; text-decoration:none; }
  a:hover { text-decoration:underline; }
</style></head><body><div class="wrap">
<pre>
   ___        ___
  |   |======/   /  ✦
  |___|     /___/      ~ ~ ~ ✦
        \  ✦  the bits leaked out
         \____________________
</pre>
<h1>💥 broken pipe</h1>
<p>no stream <code>%s</code> is flowing here.</p>
<p>maybe it ended — or never existed.</p>
<a href="/">→ see what's running</a>
</div></body></html>`
