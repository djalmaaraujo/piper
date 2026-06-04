package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"time"
)

// Tunnel exposes the local HTTP server to a wider network. Implementations
// shell out to an external CLI (tailscale / cloudflared / ngrok) — those are
// the only "dependencies", and they're optional and user-installed.
type Tunnel interface {
	Name() string
	Available() bool         // CLI present (and, where relevant, configured)
	Start(port int) (string, error) // begin tunneling, return the public URL
	Stop()
}

// startTunnel starts a specific provider by name, or auto-detects the first
// available one when name is "".
func startTunnel(name string, port int) (Tunnel, string, error) {
	providers := []Tunnel{
		&tailscaleTunnel{},
		&cloudflaredTunnel{},
		&ngrokTunnel{},
	}

	if name != "" {
		for _, p := range providers {
			if p.Name() == name {
				if !p.Available() {
					return nil, "", fmt.Errorf("%s not found or not configured (is it installed and logged in?)", name)
				}
				url, err := p.Start(port)
				return p, url, err
			}
		}
		return nil, "", fmt.Errorf("unknown provider: %s", name)
	}

	for _, p := range providers {
		if p.Available() {
			url, err := p.Start(port)
			if err == nil {
				return p, url, nil
			}
		}
	}
	return nil, "", fmt.Errorf("no tunnel provider available (install tailscale, cloudflared, or ngrok)")
}

func have(bin string) bool {
	_, err := exec.LookPath(bin)
	return err == nil
}

// stopProc kills a spawned tunnel process if it's still running.
func stopProc(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

// scanForURL reads lines from r (in the background) until one matches re,
// sending the first capture/match on the returned channel. Times out via the
// caller's select.
func scanForURL(r io.Reader, re *regexp.Regexp) <-chan string {
	out := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			if m := re.FindString(sc.Text()); m != "" {
				out <- m
				return
			}
		}
		close(out)
	}()
	return out
}

// ---------------------------------------------------------------------------
// Tailscale Funnel

type tailscaleTunnel struct{ cmd *exec.Cmd }

func (t *tailscaleTunnel) Name() string { return "tailscale" }

func (t *tailscaleTunnel) Available() bool {
	return have("tailscale") && tailscaleFunnelURL() != ""
}

func (t *tailscaleTunnel) Start(port int) (string, error) {
	url := tailscaleFunnelURL()
	if url == "" {
		return "", fmt.Errorf("could not resolve Tailscale hostname")
	}
	// `tailscale funnel <port>` proxies public 443 -> localhost:<port>,
	// running in the foreground until killed.
	t.cmd = exec.Command("tailscale", "funnel", fmt.Sprintf("%d", port))
	if err := t.cmd.Start(); err != nil {
		return "", err
	}
	return url, nil
}

func (t *tailscaleTunnel) Stop() {
	stopProc(t.cmd)
	_ = exec.Command("tailscale", "funnel", "reset").Run()
}

// ---------------------------------------------------------------------------
// Cloudflare quick tunnel (no account required)

type cloudflaredTunnel struct{ cmd *exec.Cmd }

var cfURLRe = regexp.MustCompile(`https://[a-zA-Z0-9.-]+\.trycloudflare\.com`)

func (c *cloudflaredTunnel) Name() string  { return "cloudflared" }
func (c *cloudflaredTunnel) Available() bool { return have("cloudflared") }

func (c *cloudflaredTunnel) Start(port int) (string, error) {
	c.cmd = exec.Command("cloudflared", "tunnel", "--no-autoupdate",
		"--url", fmt.Sprintf("http://localhost:%d", port))
	stderr, err := c.cmd.StderrPipe()
	if err != nil {
		return "", err
	}
	if err := c.cmd.Start(); err != nil {
		return "", err
	}
	select {
	case url, ok := <-scanForURL(stderr, cfURLRe):
		if !ok || url == "" {
			stopProc(c.cmd)
			return "", fmt.Errorf("cloudflared did not report a URL")
		}
		return url, nil
	case <-time.After(20 * time.Second):
		stopProc(c.cmd)
		return "", fmt.Errorf("timed out waiting for cloudflared URL")
	}
}

func (c *cloudflaredTunnel) Stop() { stopProc(c.cmd) }

// ---------------------------------------------------------------------------
// ngrok (requires an authtoken configured once via `ngrok config add-authtoken`)

type ngrokTunnel struct{ cmd *exec.Cmd }

func (n *ngrokTunnel) Name() string   { return "ngrok" }
func (n *ngrokTunnel) Available() bool { return have("ngrok") }

func (n *ngrokTunnel) Start(port int) (string, error) {
	n.cmd = exec.Command("ngrok", "http", fmt.Sprintf("%d", port), "--log", "stderr")
	if err := n.cmd.Start(); err != nil {
		return "", err
	}
	// ngrok exposes a local API listing active tunnels; poll it for the URL.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if url := ngrokPublicURL(); url != "" {
			return url, nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	stopProc(n.cmd)
	return "", fmt.Errorf("timed out waiting for ngrok URL (authtoken configured?)")
}

func (n *ngrokTunnel) Stop() { stopProc(n.cmd) }

func ngrokPublicURL() string {
	resp, err := http.Get("http://127.0.0.1:4040/api/tunnels")
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var body struct {
		Tunnels []struct {
			PublicURL string `json:"public_url"`
			Proto     string `json:"proto"`
		} `json:"tunnels"`
	}
	if json.NewDecoder(resp.Body).Decode(&body) != nil {
		return ""
	}
	// Prefer the https tunnel.
	for _, t := range body.Tunnels {
		if t.Proto == "https" {
			return t.PublicURL
		}
	}
	if len(body.Tunnels) > 0 {
		return body.Tunnels[0].PublicURL
	}
	return ""
}
