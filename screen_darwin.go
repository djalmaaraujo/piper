//go:build darwin

package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// piper screen captures a single macOS window and streams it as MJPEG, reusing
// the same broker/page as text streams. Window enumeration uses the system
// `swift` (CoreGraphics) once at startup; capture uses `screencapture` (+`sips`
// to downscale). No Go dependencies and no cgo — only system binaries.

type winInfo struct {
	ID    int
	App   string
	Title string
}

func (w winInfo) label() string {
	if w.Title != "" {
		return w.App + " — " + w.Title
	}
	return w.App
}

// listWindowsScript enumerates on-screen, normal-layer windows as "id\tapp\ttitle".
const listWindowsScript = `import CoreGraphics
import Foundation
let opts = CGWindowListOption(arrayLiteral: .optionOnScreenOnly, .excludeDesktopElements)
let list = CGWindowListCopyWindowInfo(opts, kCGNullWindowID) as! [[String: AnyObject]]
for w in list {
  let layer = (w[kCGWindowLayer as String] as? Int) ?? -1
  if layer != 0 { continue }
  let id = (w[kCGWindowNumber as String] as? Int) ?? -1
  let app = (w[kCGWindowOwnerName as String] as? String) ?? "?"
  let title = (w[kCGWindowName as String] as? String) ?? ""
  print("\(id)\t\(app)\t\(title)")
}`

// runSwift writes a Swift snippet to a temp file and runs it, returning stdout.
func runSwift(script string) ([]byte, error) {
	f, err := os.CreateTemp("", "piper-*.swift")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(script); err != nil {
		f.Close()
		return nil, err
	}
	f.Close()
	return exec.Command("swift", f.Name()).Output()
}

// ensureScreenPermission prompts for macOS Screen Recording access on first use
// and reports whether it's granted. Only called by `piper screen`.
func ensureScreenPermission() bool {
	out, err := runSwift("import CoreGraphics\nprint(CGRequestScreenCaptureAccess() ? \"granted\" : \"denied\")")
	if err != nil {
		return true // can't check (no swift?) — let capture try and fail loudly
	}
	return strings.TrimSpace(string(out)) == "granted"
}

func screenPermissionHelp() {
	fmt.Fprintln(os.Stderr, "  piper screen needs macOS Screen Recording permission.")
	fmt.Fprintln(os.Stderr, "  Authorize it in the dialog that just opened, or:")
	fmt.Fprintln(os.Stderr, "    System Settings → Privacy & Security → Screen Recording → enable your terminal")
	fmt.Fprintln(os.Stderr, "  then run piper screen again.")
}

func listWindows() ([]winInfo, error) {
	out, err := runSwift(listWindowsScript)
	if err != nil {
		return nil, fmt.Errorf("could not list windows (is the Xcode CLT / swift installed?): %w", err)
	}

	var wins []winInfo
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		parts := strings.SplitN(sc.Text(), "\t", 3)
		if len(parts) < 2 {
			continue
		}
		id, err := strconv.Atoi(parts[0])
		if err != nil {
			continue
		}
		w := winInfo{ID: id, App: parts[1]}
		if len(parts) == 3 {
			w.Title = parts[2]
		}
		wins = append(wins, w)
	}
	return wins, nil
}

func printWindows(wins []winInfo) {
	fmt.Fprintln(os.Stderr, "  Windows:")
	for _, w := range wins {
		fmt.Fprintf(os.Stderr, "    %s\n", w.label())
	}
	fmt.Fprintln(os.Stderr, "\n  Share one:  piper screen \"<name>\"")
}

// pickWindow matches a window by case-insensitive substring of "app — title".
func pickWindow(query string) (winInfo, []winInfo, error) {
	wins, err := listWindows()
	if err != nil {
		return winInfo{}, nil, err
	}
	if query == "" {
		return winInfo{}, wins, fmt.Errorf("name a window to share")
	}
	q := strings.ToLower(query)
	for _, w := range wins {
		if strings.Contains(strings.ToLower(w.label()), q) {
			return w, wins, nil
		}
	}
	return winInfo{}, wins, fmt.Errorf("no window matches %q", query)
}

// runScreenSource captures the chosen window into the hub as an MJPEG stream.
// Frames are written to a single temp file that is overwritten each tick and
// removed on exit, so disk use stays bounded regardless of stream length.
// asQuote escapes a string for embedding in an AppleScript string literal.
func asQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

// chooseWindow shows a native macOS picker (osascript) listing open windows.
// Used when `piper screen` is run without a window name.
func chooseWindow() (winInfo, error) {
	wins, err := listWindows()
	if err != nil {
		return winInfo{}, err
	}
	if len(wins) == 0 {
		return winInfo{}, errors.New("no windows found")
	}
	items := make([]string, len(wins))
	for i, w := range wins {
		items[i] = asQuote(fmt.Sprintf("%d: %s", i+1, w.label()))
	}
	script := fmt.Sprintf(
		`choose from list {%s} with title "piper screen" with prompt "Select a window to share:"`,
		strings.Join(items, ", "))
	out, err := exec.Command("osascript", "-e", script).Output()
	if err != nil {
		return winInfo{}, err
	}
	sel := strings.TrimSpace(string(out))
	if sel == "" || sel == "false" {
		return winInfo{}, errors.New("cancelled")
	}
	n, err := strconv.Atoi(strings.TrimSpace(strings.SplitN(sel, ":", 2)[0]))
	if err != nil || n < 1 || n > len(wins) {
		return winInfo{}, errors.New("bad selection")
	}
	return wins[n-1], nil
}

func runScreenSource(cfg *config, h *hub) int {
	if !ensureScreenPermission() {
		screenPermissionHelp()
		return 1
	}

	var win winInfo
	if cfg.screenQuery == "" {
		// No name given: show the native picker.
		w, err := chooseWindow()
		if err != nil {
			fmt.Fprintf(os.Stderr, "  %v\n", err)
			return 1
		}
		win = w
	} else {
		w, wins, err := pickWindow(cfg.screenQuery)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  Error: %v\n", err)
			if len(wins) > 0 {
				printWindows(wins)
			}
			return 1
		}
		win = w
	}

	fps := cfg.fps
	if fps < 1 {
		fps = 1
	}
	if fps > 30 {
		fps = 30
	}

	// Capture into a private 0700 dir (not a predictable /tmp path) so a local
	// attacker can't pre-create or symlink the frame file out from under us.
	tmpDir, err := os.MkdirTemp("", "piper-screen-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "  Error: %v\n", err)
		return 1
	}
	defer os.RemoveAll(tmpDir)
	tmp := filepath.Join(tmpDir, "frame.jpg")

	// Warm up: time one capture so we can report (and not promise) a real rate.
	// screencapture spawns a process per frame, so that time is the ceiling.
	warm := time.Now()
	frame, err := captureWindow(win.ID, tmp, cfg.scale, cfg.quality)
	capMs := time.Since(warm).Milliseconds()
	maxFps := 1
	if capMs > 0 {
		maxFps = int(1000 / capMs)
	}
	if maxFps < 1 {
		maxFps = 1
	}
	eff := fps
	if eff > maxFps {
		eff = maxFps
	}
	interval := time.Second / time.Duration(eff)

	note := ""
	if fps > maxFps {
		note = fmt.Sprintf("  (asked %d, capture allows ~%d)", fps, maxFps)
	}
	fmt.Fprintf(os.Stderr, "  Sharing:   %s  (%d fps%s)\n", win.label(), eff, note)

	// Send the warm-up frame immediately so viewers don't wait a full interval.
	if err == nil && len(frame) > 0 {
		h.broadcast(mjpegFrame(frame))
	}

	misses := 0
	for !h.isClosed() {
		start := time.Now()
		frame, err := captureWindow(win.ID, tmp, cfg.scale, cfg.quality)
		if err == nil && len(frame) > 0 {
			h.broadcast(mjpegFrame(frame))
			misses = 0
		} else {
			misses++
			if misses >= 10 {
				fmt.Fprintf(os.Stderr, "\n  Window closed — stopping.\n")
				return 0
			}
		}
		if d := interval - time.Since(start); d > 0 {
			time.Sleep(d)
		}
	}
	return 0
}

// captureWindow grabs one window to a temp JPEG, then shrinks it with a single
// sips pass (downscale + JPEG quality — fewer bytes, no extra process), reads
// the bytes, and removes the file.
func captureWindow(id int, tmp string, scale, quality int) ([]byte, error) {
	cmd := exec.Command("screencapture", "-x", "-o", "-l"+strconv.Itoa(id), "-t", "jpg", tmp)
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	// One sips invocation does both resize and quality so we don't spawn twice.
	args := []string{}
	if scale > 0 {
		args = append(args, "-Z", strconv.Itoa(scale))
	}
	if quality > 0 {
		args = append(args, "-s", "formatOptions", strconv.Itoa(quality))
	}
	if len(args) > 0 {
		_ = exec.Command("sips", append(args, tmp)...).Run()
	}
	data, err := os.ReadFile(tmp)
	os.Remove(tmp)
	return data, err
}

// mjpegFrame wraps a JPEG in a multipart/x-mixed-replace part.
func mjpegFrame(jpeg []byte) []byte {
	var b bytes.Buffer
	b.WriteString("--" + mjpegBoundary + "\r\n")
	b.WriteString("Content-Type: image/jpeg\r\n")
	fmt.Fprintf(&b, "Content-Length: %d\r\n\r\n", len(jpeg))
	b.Write(jpeg)
	b.WriteString("\r\n")
	return b.Bytes()
}

func runListWindows() {
	if !ensureScreenPermission() {
		screenPermissionHelp()
		os.Exit(1)
	}
	wins, err := listWindows()
	if err != nil {
		fmt.Fprintf(os.Stderr, "  Error: %v\n", err)
		os.Exit(1)
	}
	printWindows(wins)
}
