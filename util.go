package main

import "os"

// isTerminal reports whether f is an interactive character device (a TTY).
// Uses only os.FileMode — no external deps, works on all platforms.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
