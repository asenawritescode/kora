package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestS3BackendRoundTrip(t *testing.T) {
	store := map[string][]byte{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			t.Errorf("missing Authorization header on %s %s", r.Method, r.URL.Path)
		}
		key := r.URL.Path // /bucket/key
		switch r.Method {
		case http.MethodPut:
			b, _ := io.ReadAll(r.Body)
			store[key] = b
			w.WriteHeader(http.StatusOK)
		case http.MethodHead:
			b, ok := store[key]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(b)))
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			b, ok := store[key]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(b)))
			_, _ = w.Write(b)
		case http.MethodDelete:
			delete(store, key)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer srv.Close()

	b, err := New(Config{
		Backend:     "s3",
		S3Endpoint:  srv.URL,
		S3Bucket:    "bucket",
		S3AccessKey: "access",
		S3SecretKey: "secret",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	key := "sites/acme/files/2026/08/hello.txt"
	content := []byte("hello world")
	meta, err := b.Put(context.Background(), key, bytes.NewReader(content), int64(len(content)), FileMeta{Filename: "hello.txt", MIMEType: "text/plain"})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if meta.Size != int64(len(content)) {
		t.Fatalf("size = %d, want %d", meta.Size, len(content))
	}

	head, err := b.Head(context.Background(), key)
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if head.Size != int64(len(content)) {
		t.Fatalf("head size = %d, want %d", head.Size, len(content))
	}

	rc, err := b.Open(context.Background(), key, 0, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, content) {
		t.Fatalf("content = %q, want %q", got, content)
	}

	if err := b.Delete(context.Background(), key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := b.Head(context.Background(), key); err != ErrNotFound {
		t.Fatalf("Head after delete = %v, want ErrNotFound", err)
	}
}

func TestS3EnsureBucket(t *testing.T) {
	bucketExists := false
	createCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodHead:
			if r.URL.Path == "/bucket" {
				if bucketExists {
					w.WriteHeader(http.StatusOK)
				} else {
					w.WriteHeader(http.StatusNotFound)
				}
				return
			}
			w.WriteHeader(http.StatusNotFound)
		case http.MethodPut:
			if r.URL.Path == "/bucket" {
				createCalls++
				bucketExists = true
				w.WriteHeader(http.StatusOK)
				return
			}
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer srv.Close()

	b, err := New(Config{
		Backend:     "s3",
		S3Endpoint:  srv.URL,
		S3Bucket:    "bucket",
		S3AccessKey: "access",
		S3SecretKey: "secret",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := b.EnsureBucket(context.Background()); err != nil {
		t.Fatalf("EnsureBucket: %v", err)
	}
	if createCalls != 1 {
		t.Fatalf("createCalls = %d, want 1", createCalls)
	}

	if err := b.EnsureBucket(context.Background()); err != nil {
		t.Fatalf("EnsureBucket second call: %v", err)
	}
	if createCalls != 1 {
		t.Fatalf("createCalls after second call = %d, want 1", createCalls)
	}
}

func TestS3PresignURL(t *testing.T) {
	b := &s3Backend{
		endpoint:   "https://minio.example.com",
		host:       "minio.example.com",
		region:     "us-east-1",
		bucket:     "bucket",
		creds:      sigCreds{accessKey: "AKIA", secretKey: "secret", region: "us-east-1", service: "s3"},
		presignTTL: 15 * time.Minute,
	}
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	u := b.presignGet("sites/acme/files/2026/08/photo.jpg", now)

	if !strings.HasPrefix(u, "https://minio.example.com/bucket/sites/acme/files/2026/08/photo.jpg?") {
		t.Fatalf("unexpected URL prefix: %s", u)
	}
	for _, want := range []string{
		"X-Amz-Algorithm=AWS4-HMAC-SHA256",
		"X-Amz-Credential=AKIA%2F20260817%2Fus-east-1%2Fs3%2Faws4_request",
		"X-Amz-Expires=900",
		"X-Amz-SignedHeaders=host",
		"X-Amz-Signature=",
	} {
		if !strings.Contains(u, want) {
			t.Fatalf("presigned URL missing %q: %s", want, u)
		}
	}

	sig := u[strings.Index(u, "X-Amz-Signature=")+len("X-Amz-Signature="):]
	if len(sig) != 64 {
		t.Fatalf("signature length = %d, want 64 hex chars", len(sig))
	}

	if u2 := b.presignGet("sites/acme/files/2026/08/photo.jpg", now); u2 != u {
		t.Fatalf("presign not deterministic for same timestamp")
	}
}
