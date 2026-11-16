package api

import (
	"context"
	"errors"
	"io"
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
	serveStoredRange(w, r, name, contentType, int64(len(data)), func(_ context.Context, off, n int64) ([]byte, error) {
		if off >= int64(len(data)) {
			return nil, nil
		}
		return data[off:min(off+n, int64(len(data)))], nil
	})
}

// serveStoredRange is serveStoredFile for bytes read in ranges: at most
// kFileChunk of a file is in memory per request, however large the file
// and however many range requests a video player opens at once (whole
// 25 MB blobs per request took the database and the hub out of memory).
func serveStoredRange(w http.ResponseWriter, r *http.Request, name, contentType string, size int64, read session.RangeReader) {
	disposition, ctype := "attachment", "application/octet-stream"
	csp := "default-src 'none'; sandbox"
	if session.InlineType(contentType) {
		disposition, ctype = "inline", contentType
		if strings.HasPrefix(ctype, "application/pdf") {
			// The browser's PDF viewer does not run in a sandboxed document.
			csp = "default-src 'none'"
		}
	}
	if r.URL.Query().Get("download") == "1" {
		// The download button: save it, whatever the type.
		disposition = "attachment"
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
	http.ServeContent(w, r, "", time.Time{}, &chunkReader{ctx: r.Context(), size: size, read: read})
}

// kFileChunk is how much of a stored file one read fetches.
const kFileChunk = 1 << 20

// chunkReader is an io.ReadSeeker over a stored file that fetches it a
// chunk at a time (http.ServeContent seeks to a range and copies it).
type chunkReader struct {
	ctx    context.Context
	size   int64
	read   session.RangeReader
	pos    int64
	buf    []byte
	bufOff int64
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if c.pos >= c.size {
		return 0, io.EOF
	}
	if c.pos < c.bufOff || c.pos >= c.bufOff+int64(len(c.buf)) {
		data, err := c.read(c.ctx, c.pos, min(int64(kFileChunk), c.size-c.pos))
		if err != nil {
			return 0, err
		}
		if len(data) == 0 {
			return 0, io.ErrUnexpectedEOF
		}
		c.buf, c.bufOff = data, c.pos
	}
	n := copy(p, c.buf[c.pos-c.bufOff:])
	c.pos += int64(n)
	return n, nil
}

func (c *chunkReader) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = c.pos + offset
	case io.SeekEnd:
		abs = c.size + offset
	default:
		return 0, errors.New("chunkReader: bad whence")
	}
	if abs < 0 {
		return 0, errors.New("chunkReader: negative position")
	}
	c.pos = abs
	return abs, nil
}
