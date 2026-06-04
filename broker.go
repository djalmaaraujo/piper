package main

import (
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Many pipers share one port. The first to bind it becomes the host and runs
// the HTTP server, routing by stream id. Later pipers become guests: they push
// their output to the host over HTTP (POST /_ingest/<id>). If the host exits,
// the port frees and a guest promotes itself on its next loop iteration.
//
// No background daemon: the "host" is simply the first piper's own process.

type stream struct {
	id      string
	cmd     string
	pid     int
	role    string // "host" or "guest"
	started string
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
	if r.URL.Path == "/health" {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "ok")
		return
	}

	path := strings.Trim(r.URL.Path, "/")
	if path == "" {
		rg.serveIndex(w, r)
		return
	}

	parts := strings.SplitN(path, "/", 2)
	head := parts[0]

	if head == "_ingest" {
		rg.handleIngest(w, r, parts)
		return
	}

	id := head
	st, ok := rg.get(id)
	if !ok || !validID(id) {
		rg.serveBrokenPipe(w, r, id)
		return
	}
	switch {
	case len(parts) == 2 && parts[1] == "stream":
		st.hub.streamHandler(w, r)
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

// handleIngest accepts a guest's streamed output and serves it under its id.
func (rg *registry) handleIngest(w http.ResponseWriter, r *http.Request, parts []string) {
	// Ingest is for local guest pipers only — never accept output pushed from
	// the LAN or a tunnel (would let a stranger spoof/register streams).
	if !isLoopback(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost || len(parts) < 2 || parts[1] == "" {
		http.Error(w, "bad ingest", http.StatusBadRequest)
		return
	}
	id := parts[1]
	q := r.URL.Query()
	pid, _ := strconv.Atoi(q.Get("pid"))

	hb := newHub(replayLines)
	hb.echo = false // don't print a guest's output on the host's terminal
	st := &stream{
		id: id, cmd: q.Get("cmd"), pid: pid, role: "guest",
		started: q.Get("started"), hub: hb,
	}
	rg.add(st)
	defer rg.remove(id)

	buf := make([]byte, 32*1024)
	for {
		n, err := r.Body.Read(buf)
		if n > 0 {
			hb.broadcast(buf[:n])
		}
		if err != nil {
			break
		}
	}
	hb.close()
	w.WriteHeader(http.StatusOK)
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

// publish keeps this piper reachable on the shared port for its whole life.
func publish(cfg *config, id string, h *hub) {
	for {
		ln, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", cfg.port))
		if err == nil {
			becomeHost(ln, cfg, id, h) // blocks until the process exits
			return
		}
		if h.isClosed() {
			return // our source already ended; nothing to publish
		}
		if gerr := becomeGuest(cfg, id, h); gerr == nil {
			return // our stream ended cleanly
		}
		if h.isClosed() {
			return
		}
		time.Sleep(300 * time.Millisecond) // host vanished; loop may promote us
	}
}

func becomeHost(ln net.Listener, cfg *config, id string, h *hub) {
	rg := newRegistry(cfg.port)
	rg.add(&stream{
		id: id, cmd: cmdLabel(cfg), pid: os.Getpid(), role: "host",
		started: nowStamp(), hub: h,
	})
	tunnelURL := startTunnelOnce(cfg)
	printBanner(cfg, id, "host", tunnelURL)
	srv := &http.Server{Handler: rg}
	_ = srv.Serve(ln)
}

func becomeGuest(cfg *config, id string, h *hub) error {
	base := fmt.Sprintf("http://localhost:%d", cfg.port)
	q := url.Values{}
	q.Set("cmd", cmdLabel(cfg))
	q.Set("pid", strconv.Itoa(os.Getpid()))
	q.Set("started", nowStamp())
	endpoint := base + "/_ingest/" + id + "?" + q.Encode()

	pr, pw := io.Pipe()
	req, err := http.NewRequest(http.MethodPost, endpoint, pr)
	if err != nil {
		return err
	}

	ch, snap := h.register()
	go func() {
		if len(snap) > 0 {
			pw.Write(snap)
		}
		for chunk := range ch {
			if _, e := pw.Write(chunk); e != nil {
				break
			}
		}
		pw.Close()
	}()

	tunnelURL := startTunnelOnce(cfg)
	printBanner(cfg, id, "guest", tunnelURL)

	resp, err := http.DefaultClient.Do(req) // blocks until body EOF or conn drops
	h.unregister(ch)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// isLoopback reports whether the request came from 127.0.0.1/::1.
func isLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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
)

func startTunnelOnce(cfg *config) string {
	if !cfg.public {
		return ""
	}
	tunnelMu.Lock()
	defer tunnelMu.Unlock()
	if activeTunnel != nil {
		return activeTunnelURL
	}
	t, u, err := startTunnel(cfg.provider, cfg.port)
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
func cleanup() {
	stopTunnel()
}

// ---------------------------------------------------------------------------
// banner

func printBanner(cfg *config, id, role, tunnelURL string) {
	port := cfg.port
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
	fmt.Fprintf(w, "  Index:     %s/\n", base)

	viewer := base + "/" + id
	switch {
	case tunnelURL != "":
		viewer = tunnelURL + "/" + id
	case tsIP != "":
		viewer = fmt.Sprintf("http://%s:%d/%s", tsIP, port, id)
	}
	fmt.Fprintf(w, "\n  Terminal:  open %s in a browser\n", viewer)
	fmt.Fprintf(w, "  Viewers:   curl -N %s\n\n", viewer)
}

// ---------------------------------------------------------------------------
// inline pages

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
</style></head><body>
<h1>piper</h1><p class="sub">streams running on this machine</p>
<ul>%s</ul>
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
