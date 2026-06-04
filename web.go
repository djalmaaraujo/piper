package main

import (
	"embed"
	"net/http"
	"strings"
)

// webFS holds the browser UI: a single page plus vendored xterm.js assets.
// Embedded at build time, so the binary stays self-contained and works
// offline. embed is part of the standard library — no Go dependencies.
//
//go:embed web/index.html web/xterm.js web/xterm.css web/xterm-addon-fit.js
var webFS embed.FS

// wantsHTML reports whether the request looks like a browser (vs curl).
func wantsHTML(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// serveAsset serves one embedded file with a fixed content type.
func serveAsset(name, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, err := webFS.ReadFile(name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", contentType)
		w.Write(b)
	}
}
