package api

import (
	"bytes"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/dseif0x/agents-operator/internal/session"
)

// serveStoredFile sends a stored attachment. Images, video, audio and PDF
// go inline with their type, so the browser shows or plays them (with
// Range requests: iOS plays video only with those); every other file is a
// download of opaque bytes, so a browser never renders an uploaded HTML
// page or script from the hub's origin. Never let a browser guess.
func serveStoredFile(w http.ResponseWriter, r *http.Request, name, contentType string, data []byte) {
	disposition, ctype := "attachment", "application/octet-stream"
	csp := "default-src 'none'; sandbox"
	if session.InlineType(contentType) {
		disposition, ctype = "inline", contentType
		if strings.HasPrefix(ctype, "application/pdf") {
			// The browser's PDF viewer does not run in a sandboxed document.
			csp = "default-src 'none'"
		}
	}
	h := w.Header()
	h.Set("Content-Type", ctype)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", csp)
	h.Set("Cache-Control", "private, max-age=31536000, immutable")
	if cd := mime.FormatMediaType(disposition, map[string]string{"filename": session.CleanFileName(name)}); cd != "" && name != "" {
		h.Set("Content-Disposition", cd)
	} else {
		h.Set("Content-Disposition", disposition)
	}
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
}
