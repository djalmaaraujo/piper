//go:build darwin

package main

import (
	"bufio"
	"bytes"
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

func listWindows() ([]winInfo, error) {
	f, err := os.CreateTemp("", "piper-windows-*.swift")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(listWindowsScript); err != nil {
		f.Close()
		return nil, err
	}
	f.Close()

	out, err := exec.Command("swift", f.Name()).Output()
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
func runScreenSource(cfg *config, h *hub) int {
	win, wins, err := pickWindow(cfg.screenQuery)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  Error: %v\n", err)
		if len(wins) > 0 {
			printWindows(wins)
		}
		return 1
	}

	fps := cfg.fps
	if fps < 1 {
		fps = 1
	}
	if fps > 30 {
		fps = 30
	}
	interval := time.Second / time.Duration(fps)

	tmp := filepath.Join(os.TempDir(), fmt.Sprintf("piper-screen-%d.jpg", os.Getpid()))
	defer os.Remove(tmp)

	fmt.Fprintf(os.Stderr, "  Sharing:   %s  (%d fps)\n", win.label(), fps)

	misses := 0
	for !h.isClosed() {
		start := time.Now()
		if frame, err := captureWindow(win.ID, tmp, cfg.scale); err == nil && len(frame) > 0 {
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

// captureWindow grabs one window to a temp JPEG, optionally downscales it with
// sips, reads the bytes, then removes the file.
func captureWindow(id int, tmp string, scale int) ([]byte, error) {
	cmd := exec.Command("screencapture", "-x", "-o", "-l"+strconv.Itoa(id), "-t", "jpg", tmp)
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	if scale > 0 {
		_ = exec.Command("sips", "-Z", strconv.Itoa(scale), tmp).Run()
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
	wins, err := listWindows()
	if err != nil {
		fmt.Fprintf(os.Stderr, "  Error: %v\n", err)
		os.Exit(1)
	}
	printWindows(wins)
}
