package storage

import "strings"

// extMIME maps file extensions to content types for serving uploads with correct headers.
var extMIME = map[string]string{
	".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".png": "image/png",
	".gif": "image/gif", ".webp": "image/webp", ".bmp": "image/bmp",
	".svg": "image/svg+xml", ".avif": "image/avif",
	".pdf": "application/pdf",
	".mp3": "audio/mpeg", ".wav": "audio/wav", ".ogg": "audio/ogg",
	".oga": "audio/ogg", ".m4a": "audio/mp4", ".aac": "audio/aac",
	".flac": "audio/flac", ".opus": "audio/opus", ".webm": "audio/webm",
	".mp4": "video/mp4", ".mov": "video/quicktime",
	".txt": "text/plain", ".csv": "text/csv", ".md": "text/markdown",
	".json": "application/json", ".zip": "application/zip",
}

// MimeByExt returns a content type for a file extension, or "" if unknown.
func MimeByExt(ext string) string {
	return extMIME[strings.ToLower(ext)]
}

// IsInlineSafe reports whether a file extension is safe to render inline in a browser.
func IsInlineSafe(ext string) bool {
	m := MimeByExt(ext)
	if strings.HasPrefix(m, "image/") || strings.HasPrefix(m, "audio/") || strings.HasPrefix(m, "video/") {
		return true
	}
	switch strings.ToLower(ext) {
	case ".pdf", ".txt", ".csv", ".md", ".json":
		return true
	}
	return false
}
