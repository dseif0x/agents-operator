package api

import (
	"bytes"
	"context"
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
	// The download button: an attachment, still with its type.
	r := httptest.NewRequest(http.MethodGet, "/f?download=1", nil)
	dw := httptest.NewRecorder()
	serveStoredFile(dw, r, "clip.mp4", "video/mp4", []byte("0123456789"))
	if dw.Header().Get("Content-Disposition") != `attachment; filename=clip.mp4` || dw.Body.String() != "0123456789" {
		t.Fatalf("download: %v", dw.Header())
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

// A stored file is read a chunk at a time, never whole: a range fetches
// one chunk from where it starts, a full download streams chunk by chunk.
func TestServeStoredRangeChunks(t *testing.T) {
	data := make([]byte, 3*kFileChunk+123)
	for i := range data {
		data[i] = byte(i * 7)
	}
	var reads []int64
	read := func(_ context.Context, off, n int64) ([]byte, error) {
		reads = append(reads, n)
		if n > kFileChunk {
			t.Fatalf("read of %d bytes", n)
		}
		return data[off:min(off+n, int64(len(data)))], nil
	}
	get := func(rng string) *httptest.ResponseRecorder {
		reads = nil
		r := httptest.NewRequest(http.MethodGet, "/f", nil)
		if rng != "" {
			r.Header.Set("Range", rng)
		}
		w := httptest.NewRecorder()
		serveStoredRange(w, r, "clip.mp4", "video/mp4", int64(len(data)), read)
		return w
	}
	w := get("bytes=1048570-1048590")
	if w.Code != http.StatusPartialContent || !bytes.Equal(w.Body.Bytes(), data[1048570:1048591]) || len(reads) != 1 {
		t.Fatalf("small range: %d, %d bytes, reads %v", w.Code, w.Body.Len(), reads)
	}
	w = get("bytes=-100")
	if w.Code != http.StatusPartialContent || !bytes.Equal(w.Body.Bytes(), data[len(data)-100:]) || len(reads) != 1 {
		t.Fatalf("suffix range: %d, reads %v", w.Code, reads)
	}
	w = get("")
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), data) || len(reads) != 4 {
		t.Fatalf("whole file: %d, %d bytes, reads %v", w.Code, w.Body.Len(), reads)
	}
}
