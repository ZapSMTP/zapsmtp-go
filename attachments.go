package zapsmtp

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
)

var contentTypes = map[string]string{
	".pdf": "application/pdf", ".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif",
	".webp": "image/webp", ".svg": "image/svg+xml", ".txt": "text/plain", ".csv": "text/csv", ".html": "text/html",
	".ics": "text/calendar", ".json": "application/json", ".xml": "application/xml", ".zip": "application/zip",
	".doc":  "application/msword",
	".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".xls":  "application/vnd.ms-excel",
	".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
}

// AttachmentFromBytes returns an attachment of content, base64-encoded. Its
// content type follows the file name's extension when the SDK knows it; set
// ContentType (and ContentID for an inline image) on the result to change that.
func AttachmentFromBytes(filename string, content []byte) OutgoingAttachment {
	a := OutgoingAttachment{Filename: filename, Content: String(base64.StdEncoding.EncodeToString(content))}
	if t, ok := contentTypes[strings.ToLower(filepath.Ext(filename))]; ok {
		a.ContentType = String(t)
	}
	return a
}

// AttachmentFromFile reads a file into an attachment named after it (see AttachmentFromBytes).
func AttachmentFromFile(path string) (OutgoingAttachment, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return OutgoingAttachment{}, err
	}
	return AttachmentFromBytes(filepath.Base(path), b), nil
}
