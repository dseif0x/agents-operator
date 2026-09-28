//go:build !ui

// Package ui embeds the built frontend. Without the `ui` build tag this
// stub is compiled instead, so backend builds and tests never need Node.
package ui

import "net/http"

// Built reports whether the binary carries the frontend.
const Built = false

// Handler returns 503 for every page: the binary was built without -tags ui.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("agenthub was built without the web UI (go build -tags ui). The API is available under /api/v1.\n"))
	})
}
