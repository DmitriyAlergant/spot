package main

import (
	"fmt"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strings"
)

func uploadContentType(filename, declared string, file io.ReadSeeker) (string, error) {
	if contentType := preferredUploadContentType(filename, declared); contentType != "" {
		return contentType, nil
	}

	sniff := make([]byte, 512)
	n, _ := io.ReadFull(file, sniff)
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("seek upload: %w", err)
	}
	if n == 0 {
		return "application/octet-stream", nil
	}
	return canonicalContentType(http.DetectContentType(sniff[:n])), nil
}

func preferredUploadContentType(filename, declared string) string {
	if contentType := declaredUploadContentType(declared); contentType != "" {
		return contentType
	}
	return contentTypeByExtension(filename)
}

func declaredUploadContentType(declared string) string {
	contentType := canonicalContentType(declared)
	if contentType == "" || isGenericUploadContentType(contentType) {
		return ""
	}
	return contentType
}

func contentTypeForStorage(filename, contentType string) string {
	contentType = canonicalContentType(contentType)
	if contentType != "" && !isGenericUploadContentType(contentType) {
		return contentType
	}
	if contentType := contentTypeByExtension(filename); contentType != "" {
		return contentType
	}
	return "application/octet-stream"
}

func contentTypeForRead(filename, stored string) string {
	contentType := canonicalContentType(stored)
	if contentType == "" || isGenericUploadContentType(contentType) {
		if byExtension := contentTypeByExtension(filename); byExtension != "" {
			return byExtension
		}
	} else if legacyPlainTextDefault(contentType) {
		if byExtension := contentTypeByExtension(filename); byExtension != "" && !strings.HasPrefix(byExtension, "text/") {
			return byExtension
		}
	}
	if contentType != "" {
		return contentType
	}
	if contentType := contentTypeByExtension(filename); contentType != "" {
		return contentType
	}
	return "application/octet-stream"
}

func contentTypeByExtension(filename string) string {
	return canonicalContentType(mime.TypeByExtension(filepath.Ext(filename)))
}

func canonicalContentType(contentType string) string {
	media, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		media = contentType
		if i := strings.IndexByte(media, ';'); i >= 0 {
			media = media[:i]
		}
	}
	media = strings.ToLower(strings.TrimSpace(media))
	if media == "" {
		return ""
	}
	if strings.HasPrefix(media, "text/") {
		return mime.FormatMediaType(media, map[string]string{"charset": "utf-8"})
	}
	return media
}

func isGenericUploadContentType(contentType string) bool {
	media, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		media = contentType
	}
	return strings.EqualFold(strings.TrimSpace(media), "application/octet-stream")
}

func legacyPlainTextDefault(contentType string) bool {
	media, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		media = contentType
	}
	return strings.EqualFold(strings.TrimSpace(media), "text/plain")
}
