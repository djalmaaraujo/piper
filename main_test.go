package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseArgs(t *testing.T) {
	t.Run("command with default port", func(t *testing.T) {
		c, err := parseArgs([]string{"echo", "hi"})
		if err != nil {
			t.Fatal(err)
		}
		if c.port != defaultPort {
			t.Errorf("port = %d, want %d", c.port, defaultPort)
		}
		if !reflect.DeepEqual(c.command, []string{"echo", "hi"}) {
			t.Errorf("command = %v", c.command)
		}
		if c.pipe || c.help {
			t.Errorf("pipe=%v help=%v, want both false", c.pipe, c.help)
		}
	})

	t.Run("custom port", func(t *testing.T) {
		c, err := parseArgs([]string{"--port", "8080", "ls"})
		if err != nil {
			t.Fatal(err)
		}
		if c.port != 8080 || !reflect.DeepEqual(c.command, []string{"ls"}) {
			t.Errorf("port=%d command=%v", c.port, c.command)
		}
	})

	t.Run("invalid port value", func(t *testing.T) {
		if _, err := parseArgs([]string{"--port", "abc", "ls"}); err == nil {
			t.Error("want error for non-numeric port")
		}
	})

	t.Run("port without value", func(t *testing.T) {
		if _, err := parseArgs([]string{"--port"}); err == nil {
			t.Error("want error for missing port value")
		}
	})

	t.Run("port out of range", func(t *testing.T) {
		if _, err := parseArgs([]string{"--port", "99999", "ls"}); err == nil {
			t.Error("want error for out-of-range port")
		}
	})

	t.Run("PORT env", func(t *testing.T) {
		t.Setenv("PORT", "7000")
		c, err := parseArgs([]string{"echo"})
		if err != nil {
			t.Fatal(err)
		}
		if c.port != 7000 {
			t.Errorf("port = %d, want 7000", c.port)
		}
	})

	t.Run("public auto-detect", func(t *testing.T) {
		c, _ := parseArgs([]string{"--public", "cmd"})
		if !c.public || c.provider != "" {
			t.Errorf("public=%v provider=%q", c.public, c.provider)
		}
	})

	t.Run("forced providers", func(t *testing.T) {
		for _, p := range []string{"tailscale", "cloudflared", "ngrok"} {
			c, _ := parseArgs([]string{"--" + p, "cmd"})
			if !c.public || c.provider != p {
				t.Errorf("--%s: public=%v provider=%q", p, c.public, c.provider)
			}
		}
	})

	t.Run("help and version", func(t *testing.T) {
		if c, _ := parseArgs([]string{"-h"}); !c.help {
			t.Error("-h should set help")
		}
		if c, _ := parseArgs([]string{"--version"}); !c.showVersion {
			t.Error("--version should set showVersion")
		}
	})

	t.Run("no command is pipe mode", func(t *testing.T) {
		c, err := parseArgs(nil)
		if err != nil {
			t.Fatal(err)
		}
		if !c.pipe {
			t.Error("no args should be pipe mode")
		}
	})
}

func TestShellJoin(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{[]string{"echo", "hi"}, `'echo' 'hi'`},
		{[]string{"bash", "-c", "echo hi"}, `'bash' '-c' 'echo hi'`},
		{[]string{"a'b"}, `'a'\''b'`},
	}
	for _, c := range cases {
		if got := shellJoin(c.in); got != c.want {
			t.Errorf("shellJoin(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestExitCodeOf(t *testing.T) {
	if got := exitCodeOf(nil); got != 0 {
		t.Errorf("nil err = %d, want 0", got)
	}
	err := exec.Command("sh", "-c", "exit 3").Run()
	if got := exitCodeOf(err); got != 3 {
		t.Errorf("exit 3 = %d, want 3", got)
	}
}

func TestReplayBuffer(t *testing.T) {
	h := newHub(2) // keep only last 2 lines
	h.appendReplay([]byte("a\nb\nc\n"))
	_, snap := h.register()
	if string(snap) != "b\nc\n" {
		t.Errorf("snapshot = %q, want %q", snap, "b\nc\n")
	}

	h2 := newHub(10)
	h2.appendReplay([]byte("done\npartial-no-newline"))
	_, snap2 := h2.register()
	if !strings.Contains(string(snap2), "partial-no-newline") {
		t.Errorf("snapshot should include partial line, got %q", snap2)
	}
}

func TestBroadcastDelivers(t *testing.T) {
	silenceStdout(t)
	h := newHub(100)
	ch, _ := h.register()
	h.broadcast([]byte("hello"))
	select {
	case got := <-ch:
		if string(got) != "hello" {
			t.Errorf("got %q, want hello", got)
		}
	case <-time.After(time.Second):
		t.Fatal("no chunk delivered")
	}
}

func TestSlowClientDropped(t *testing.T) {
	silenceStdout(t)
	h := newHub(100)
	ch, _ := h.register()
	// Fill the client's buffer past capacity so the next broadcast drops it.
	for i := 0; i < clientBufSize+10; i++ {
		h.broadcast([]byte("x"))
	}
	h.mu.Lock()
	_, stillRegistered := h.clients[ch]
	h.mu.Unlock()
	if stillRegistered {
		t.Error("slow client should have been dropped")
	}
}

func TestHTTPRouting(t *testing.T) {
	silenceStdout(t)
	t.Setenv("PIPER_CONFIG", filepath.Join(t.TempDir(), "piper-config.json"))
	rg := newRegistry(9999)
	h := newHub(100)
	h.appendReplay([]byte("replayed-line\n"))
	rg.add(&stream{id: "ABC123", cmd: "echo hi", role: "host", manager: true, hub: h})
	srv := httptest.NewServer(rg)
	defer srv.Close()

	t.Run("health", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/health")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if string(body) != "ok" {
			t.Errorf("health body = %q", body)
		}
	})

	t.Run("index lists streams", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if !strings.Contains(string(body), "ABC123") {
			t.Errorf("index should list the stream id, got %q", body)
		}
	})

	t.Run("index disabled without --manager", func(t *testing.T) {
		t.Setenv("PIPER_CONFIG", filepath.Join(t.TempDir(), "c.json"))
		rg2 := newRegistry(9998)
		rg2.add(&stream{id: "NOMGR0", cmd: "echo hi", role: "host", hub: newHub(10)})
		s2 := httptest.NewServer(rg2)
		defer s2.Close()
		resp, err := http.Get(s2.URL + "/")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("index should be 404 without --manager, got %d", resp.StatusCode)
		}
		body, _ := io.ReadAll(resp.Body)
		if strings.Contains(string(body), "NOMGR0") {
			t.Error("disabled index must not leak stream ids")
		}
	})

	t.Run("browser gets HTML page at /<id>", func(t *testing.T) {
		req, _ := http.NewRequest("GET", srv.URL+"/ABC123", nil)
		req.Header.Set("Accept", "text/html")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
			t.Errorf("content-type = %q", ct)
		}
		body, _ := io.ReadAll(resp.Body)
		if !strings.Contains(string(body), `id="log"`) {
			t.Error("HTML should contain the log element")
		}
		if strings.Contains(string(body), "xterm") {
			t.Error("HTML must not reference xterm (dependency removed)")
		}
	})

	t.Run("curl gets raw stream with replay at /<id>/stream", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/ABC123/stream", nil)
		req.Header.Set("Accept", "*/*")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/plain") {
			t.Errorf("content-type = %q, want text/plain", ct)
		}
		buf := make([]byte, len("replayed-line\n"))
		if _, err := io.ReadFull(resp.Body, buf); err != nil {
			t.Fatal(err)
		}
		if string(buf) != "replayed-line\n" {
			t.Errorf("stream replay = %q", buf)
		}
	})

	t.Run("unknown id => broken pipe 404", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/NOPE99")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("status = %d, want 404", resp.StatusCode)
		}
		body, _ := io.ReadAll(resp.Body)
		if !strings.Contains(strings.ToLower(string(body)), "broken pipe") {
			t.Errorf("expected broken-pipe body, got %q", body)
		}
	})
}

func TestIDGen(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		id := genID()
		if len(id) != 6 || !validID(id) {
			t.Fatalf("bad id: %q", id)
		}
		seen[id] = true
	}
	if len(seen) < 190 { // collisions should be rare
		t.Errorf("too many collisions: %d unique of 200", len(seen))
	}
}

// silenceStdout redirects os.Stdout to /dev/null for the duration of a test,
// since broadcast() echoes to the real terminal.
func silenceStdout(t *testing.T) {
	t.Helper()
	old := os.Stdout
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = devnull
	t.Cleanup(func() {
		os.Stdout = old
		devnull.Close()
	})
}

func TestBrokerRouting(t *testing.T) {
	silenceStdout(t)
	t.Setenv("PIPER_CONFIG", filepath.Join(t.TempDir(), "piper-config.json"))
	rg := newRegistry(9999)
	h := newHub(100)
	rg.add(&stream{id: "ABC123", cmd: "echo hi", pid: 1, role: "host", manager: true, hub: h})
	srv := httptest.NewServer(rg)
	defer srv.Close()

	t.Run("POST is rejected (no write surface)", func(t *testing.T) {
		resp, err := http.Post(srv.URL+"/ABC123", "text/plain", strings.NewReader("evil"))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("POST status = %d, want 405", resp.StatusCode)
		}
	})

	t.Run("unknown id -> broken pipe 404", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/ZZZZZZ")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("status = %d, want 404", resp.StatusCode)
		}
		body, _ := io.ReadAll(resp.Body)
		if !strings.Contains(strings.ToLower(string(body)), "broken pipe") {
			t.Errorf("expected a broken-pipe message, got %q", body)
		}
	})

	t.Run("index lists the stream", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if !strings.Contains(string(body), "ABC123") {
			t.Errorf("index missing stream id, got %q", body)
		}
	})

	t.Run("browser gets HTML page at /id", func(t *testing.T) {
		req, _ := http.NewRequest("GET", srv.URL+"/ABC123", nil)
		req.Header.Set("Accept", "text/html")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if !strings.Contains(string(body), `id="log"`) {
			t.Error("expected the terminal page at /id")
		}
	})

	t.Run("curl gets raw stream at /id/stream", func(t *testing.T) {
		h.appendReplay([]byte("hello-stream\n"))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/ABC123/stream", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		buf := make([]byte, len("hello-stream\n"))
		if _, err := io.ReadFull(resp.Body, buf); err != nil {
			t.Fatal(err)
		}
		if string(buf) != "hello-stream\n" {
			t.Errorf("stream = %q", buf)
		}
	})
}
