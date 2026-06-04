package main

import (
	"embed"
	"net/http"
	"strings"
)

// webFS holds the browser UI: a single self-contained HTML page (no JS
// libraries — just a fetch loop into a <pre>). Embedded at build time, so the
// binary stays self-contained. embed is standard library — no Go dependencies.
//
//go:embed web/index.html
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
