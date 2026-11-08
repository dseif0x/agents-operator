// Package filetype names the content type of a file from its bytes and,
// where sniffing cannot tell (QuickTime video, most audio, Markdown, logs),
// from its name. The table is built in: container images often have no
// /etc/mime.types.
package filetype

import (
	"net/http"
	"path/filepath"
	"strings"
)

var byExt = map[string]string{
	".mov": "video/quicktime", ".mp4": "video/mp4", ".m4v": "video/mp4", ".webm": "video/webm",
	".mp3": "audio/mpeg", ".m4a": "audio/mp4", ".aac": "audio/aac", ".wav": "audio/wav",
	".ogg": "audio/ogg", ".oga": "audio/ogg", ".opus": "audio/ogg", ".flac": "audio/flac",
	".pdf": "application/pdf",
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif", ".webp": "image/webp",
	".txt": "text/plain; charset=utf-8", ".log": "text/plain; charset=utf-8", ".md": "text/markdown; charset=utf-8",
	".csv": "text/csv; charset=utf-8", ".json": "application/json", ".yaml": "application/yaml", ".yml": "application/yaml",
	".zip": "application/zip", ".gz": "application/gzip", ".tar": "application/x-tar",
}

// Detect returns the content type of a file named name with content data:
// the sniffed type, unless sniffing only says "some bytes" or "some text"
// and the name says more.
func Detect(name string, data []byte) string {
	ct := http.DetectContentType(data)
	if ct != "application/octet-stream" && !strings.HasPrefix(ct, "text/plain") {
		return ct
	}
	if t, ok := byExt[strings.ToLower(filepath.Ext(name))]; ok {
		// Text by name only when the bytes are text, media only when they
		// are not: a .txt that is binary stays opaque.
		isText := strings.HasPrefix(ct, "text/plain")
		if isText == (strings.HasPrefix(t, "text/") || strings.HasPrefix(t, "application/json") || strings.HasPrefix(t, "application/yaml")) {
			return t
		}
	}
	return ct
}
