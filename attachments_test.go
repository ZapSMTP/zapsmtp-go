package zapsmtp

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAttachmentFromBytes(t *testing.T) {
	a := AttachmentFromBytes("a.pdf", []byte{1, 2})
	if a.Filename != "a.pdf" || *a.Content != "AQI=" || *a.ContentType != "application/pdf" || a.ContentID != nil {
		t.Fatalf("%+v", a)
	}
	if a := AttachmentFromBytes("x.unknown", []byte{1}); a.ContentType != nil {
		t.Fatal(*a.ContentType)
	}
}

func TestAttachmentFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "Report.PNG")
	mustNil(t, os.WriteFile(path, []byte{137, 80}, 0o600))
	a, err := AttachmentFromFile(path)
	mustNil(t, err)
	if a.Filename != "Report.PNG" || *a.Content != "iVA=" || *a.ContentType != "image/png" {
		t.Fatalf("%+v", a)
	}
	if _, err := AttachmentFromFile(filepath.Join(dir, "missing.txt")); !os.IsNotExist(err) {
		t.Fatal(err)
	}
}
