package server

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadFileRelative(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "report.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := &Chat{Roots: []string{ws}}
	f, err := c.readFileMax("report.txt", 25<<20)
	if err != nil || f.Name != "report.txt" || string(f.Data) != "hello" || f.ContentType != "text/plain; charset=utf-8" {
		t.Fatalf("file = %+v, %v", f, err)
	}
	if _, err := c.readFileMax("../outside.txt", 25<<20); err == nil {
		t.Fatal("a file outside the workspace was read")
	}
}
