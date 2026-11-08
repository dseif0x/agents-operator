package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestServeStoredFile(t *testing.T) {
	get := func(name, ct string, data []byte, rng string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/f", nil)
		if rng != "" {
			r.Header.Set("Range", rng)
		}
		w := httptest.NewRecorder()
		serveStoredFile(w, r, name, ct, data)
		return w
	}
	// Video: inline with its type, and Range works (iOS needs it).
	w := get("clip.mp4", "video/mp4", []byte("0123456789"), "bytes=2-5")
	if w.Code != http.StatusPartialContent || w.Body.String() != "2345" || w.Header().Get("Content-Type") != "video/mp4" ||
		w.Header().Get("Content-Disposition") != `inline; filename=clip.mp4` || w.Header().Get("Accept-Ranges") != "bytes" {
		t.Fatalf("video: %d %q %v", w.Code, w.Body.String(), w.Header())
	}
	// HTML or SVG never render in the hub's origin.
	for _, ct := range []string{"text/html; charset=utf-8", "image/svg+xml"} {
		w = get("x", ct, []byte("<script>alert(1)</script>"), "")
		if w.Header().Get("Content-Type") != "application/octet-stream" || w.Header().Get("Content-Disposition") != "attachment; filename=x" ||
			w.Header().Get("Content-Security-Policy") != "default-src 'none'; sandbox" {
			t.Fatalf("%s: %v", ct, w.Header())
		}
	}
	// PDF: inline, not sandboxed (the viewer would not load), no sources.
	w = get("doc.pdf", "application/pdf", []byte("%PDF-1.7"), "")
	if w.Header().Get("Content-Type") != "application/pdf" || w.Header().Get("Content-Security-Policy") != "default-src 'none'" {
		t.Fatalf("pdf: %v", w.Header())
	}
	// Text: a download (the UI previews it by fetching the bytes).
	w = get("notes.txt", "text/plain; charset=utf-8", []byte("hi"), "")
	if w.Header().Get("Content-Type") != "application/octet-stream" || w.Body.String() != "hi" {
		t.Fatalf("text: %v", w.Header())
	}
}
