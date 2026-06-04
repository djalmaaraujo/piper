//go:build !darwin

package main

import (
	"fmt"
	"os"
)

// Screen sharing relies on macOS tools (screencapture / CoreGraphics).

func runScreenSource(cfg *config, h *hub) int {
	fmt.Fprintln(os.Stderr, "  Error: piper screen is only supported on macOS.")
	return 1
}

func runListWindows() {
	fmt.Fprintln(os.Stderr, "  Error: piper screen is only supported on macOS.")
	os.Exit(1)
}
