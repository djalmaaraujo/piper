package main

import (
	"fmt"
	"net"
	"os"
)

// isTerminal reports whether f is an interactive character device (a TTY).
// Uses only os.FileMode — no external deps, works on all platforms.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// newListener binds 0.0.0.0:port so LAN/Tailscale peers can reach the stream.
func newListener(port int) (net.Listener, error) {
	addr := fmt.Sprintf("0.0.0.0:%d", port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("cannot listen on %s: %w", addr, err)
	}
	return ln, nil
}
