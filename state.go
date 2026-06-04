package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// stateFile records which pipers are currently sharing the port. It's written
// by whichever piper owns the port (the host) and read by `piper --list`.
// Lives at ~/piper-config.json.
type stateFile struct {
	Port   int          `json:"port"`
	Pipers []piperEntry `json:"pipers"`
}

type piperEntry struct {
	ID      string `json:"id"`
	Cmd     string `json:"cmd"`
	PID     int    `json:"pid"`
	Started string `json:"started"`
	Role    string `json:"role"` // "host" or "guest"
}

func statePath() string {
	if p := os.Getenv("PIPER_CONFIG"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "piper-config.json"
	}
	return filepath.Join(home, "piper-config.json")
}

// writeState atomically writes the state file (temp + rename).
func writeState(s stateFile) error {
	path := statePath()
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	// 0600: the file lists other pipers' command lines + PIDs.
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readState() (stateFile, error) {
	var s stateFile
	b, err := os.ReadFile(statePath())
	if err != nil {
		return s, err
	}
	err = json.Unmarshal(b, &s)
	return s, err
}

func nowStamp() string { return time.Now().Format("2006-01-02 15:04:05") }

// runList prints the pipers currently registered, verifying the host is alive.
func runList(port int) {
	s, err := readState()
	if err != nil || len(s.Pipers) == 0 {
		fmt.Println("No pipers running.")
		return
	}

	alive := hostAlive(s.Port)
	status := "alive"
	if !alive {
		status = "stale (no host responding — file may be outdated)"
	}
	fmt.Printf("Pipers on port %d (%s):\n\n", s.Port, status)
	fmt.Printf("  %-8s %-6s %-8s %-19s %s\n", "ID", "ROLE", "PID", "STARTED", "COMMAND")
	for _, p := range s.Pipers {
		fmt.Printf("  %-8s %-6s %-8d %-19s %s\n", p.ID, p.Role, p.PID, p.Started, p.Cmd)
	}
	fmt.Printf("\n  Open:  http://localhost:%d/\n", s.Port)
}

// hostAlive reports whether something is serving piper on the port.
func hostAlive(port int) bool {
	c := &http.Client{Timeout: 800 * time.Millisecond}
	resp, err := c.Get(fmt.Sprintf("http://localhost:%d/health", port))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
