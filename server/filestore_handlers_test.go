package main

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"testing"
	"time"
)

// TestFileListAndDeleteHandlers exercises the new files endpoints end to
// end against a local store: upload, list, delete via the same path the
// SDK uses, and confirm the deleted upload is gone.
func TestFileListAndDeleteHandlers(t *testing.T) {
	files, err := NewLocalFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		files:      files,
		policies:   NewPolicyStore(t.TempDir(), time.Second),
		spotDomain: "spot.localhost",
	}

	upload := func(filename, content string) StoredFile {
		t.Helper()
		var form bytes.Buffer
		writer := multipart.NewWriter(&form)
		part, err := writer.CreateFormFile("file", filename)
		if err != nil {
			t.Fatal(err)
		}
		part.Write([]byte(content))
		writer.Close()
		req := httptest.NewRequest(http.MethodPost, "http://demo.spot.localhost/api/files", &form)
		req.Header.Set("Content-Type", writer.FormDataContentType())
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("upload %s: status %d body %s", filename, rec.Code, rec.Body)
		}
		var stored StoredFile
		if err := json.Unmarshal(rec.Body.Bytes(), &stored); err != nil {
			t.Fatalf("decode upload: %v", err)
		}
		return stored
	}

	list := func() []StoredFile {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "http://demo.spot.localhost/api/files", nil)
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("list: status %d body %s", rec.Code, rec.Body)
		}
		var out struct {
			Files []StoredFile `json:"files"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode list: %v", err)
		}
		return out.Files
	}

	a := upload("a.txt", "aaa")
	upload("b.txt", "bb")

	if got := list(); len(got) != 2 {
		t.Fatalf("list = %d files, want 2", len(got))
	}

	// Delete using the path shape the SDK derives from the stored URL.
	del := httptest.NewRequest(http.MethodDelete,
		"http://demo.spot.localhost/api/files/"+a.ID+"/"+a.Name, nil)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, del)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: status %d body %s", rec.Code, rec.Body)
	}

	if got := list(); len(got) != 1 || got[0].Name != "b.txt" {
		t.Fatalf("list after delete = %+v, want only b.txt", got)
	}

	// Downloading the deleted upload now 404s.
	dl := httptest.NewRequest(http.MethodGet,
		"http://demo.spot.localhost/api/files/demo/"+a.ID+"/"+a.Name, nil)
	rec = httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, dl)
	if rec.Code != http.StatusNotFound {
		t.Errorf("download deleted = %d, want 404", rec.Code)
	}

	// A malformed id is rejected before the store is touched.
	badDel := httptest.NewRequest(http.MethodDelete,
		"http://demo.spot.localhost/api/files/not-an-id/x.txt", nil)
	rec = httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, badDel)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("delete with bad id = %d, want 400", rec.Code)
	}
}

func TestFileUploadServesContentTypes(t *testing.T) {
	files, err := NewLocalFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		files:      files,
		policies:   NewPolicyStore(t.TempDir(), time.Second),
		spotDomain: "spot.localhost",
	}

	cases := []struct {
		name        string
		filename    string
		declared    string
		body        []byte
		contentType string
	}{
		{"png", "probe.png", "image/png", []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"), "image/png"},
		{"svg", "probe.svg", "image/svg+xml", []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="10" height="10"/>`), "image/svg+xml"},
		{"pdf", "probe.pdf", "application/pdf", []byte("%PDF-1.7\n"), "application/pdf"},
		{"text", "probe.txt", "text/plain", []byte("hello"), "text/plain; charset=utf-8"},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			var form bytes.Buffer
			writer := multipart.NewWriter(&form)
			header := make(textproto.MIMEHeader)
			header.Set("Content-Disposition", `form-data; name="file"; filename="`+tt.filename+`"`)
			header.Set("Content-Type", tt.declared)
			part, err := writer.CreatePart(header)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := part.Write(tt.body); err != nil {
				t.Fatal(err)
			}
			writer.Close()

			req := httptest.NewRequest(http.MethodPost, "http://demo.spot.localhost/api/files", &form)
			req.Header.Set("Content-Type", writer.FormDataContentType())
			rec := httptest.NewRecorder()
			srv.routes().ServeHTTP(rec, req)
			if rec.Code != http.StatusCreated {
				t.Fatalf("upload: status %d body %s", rec.Code, rec.Body)
			}
			var stored StoredFile
			if err := json.Unmarshal(rec.Body.Bytes(), &stored); err != nil {
				t.Fatalf("decode upload: %v", err)
			}
			if stored.ContentType != tt.contentType {
				t.Fatalf("upload content_type = %q, want %q", stored.ContentType, tt.contentType)
			}

			req = httptest.NewRequest(http.MethodGet, "http://demo.spot.localhost"+stored.URL, nil)
			rec = httptest.NewRecorder()
			srv.routes().ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("download: status %d body %s", rec.Code, rec.Body)
			}
			if got := rec.Header().Get("Content-Type"); got != tt.contentType {
				t.Fatalf("download Content-Type = %q, want %q", got, tt.contentType)
			}
		})
	}
}
