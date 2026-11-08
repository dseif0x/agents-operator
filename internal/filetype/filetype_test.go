package filetype

import "testing"

func TestDetect(t *testing.T) {
	mov := append([]byte("\x00\x00\x00\x14ftypqt  \x00\x00\x00\x00qt  "), make([]byte, 64)...)
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"IMG_0001.MOV", mov, "video/quicktime"},
		{"notes.md", []byte("# Title\nhello"), "text/markdown; charset=utf-8"},
		{"run.log", []byte("ok\n"), "text/plain; charset=utf-8"},
		{"a.png", []byte("\x89PNG\r\n\x1a\n...."), "image/png"},
		{"fake.mov", []byte("just text"), "text/plain; charset=utf-8"}, // text stays text
		{"blob.txt", []byte{0, 1, 2, 3, 0xff, 0xfe}, "application/octet-stream"},
		{"page.html", []byte("<html><body>x"), "text/html; charset=utf-8"},
	}
	for _, c := range cases {
		if got := Detect(c.name, c.data); got != c.want {
			t.Errorf("Detect(%q) = %q, want %q", c.name, got, c.want)
		}
	}
}
