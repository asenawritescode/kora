// Package storage provides a pluggable backend for file attachments.
// Implementations: local filesystem and S3-compatible object storage (MinIO,
// AWS S3, Cloudflare R2, Garage, and other S3-compatible Rust/self-hosted stores).
package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"
)

// FileMeta describes a stored object. It travels with the blob reference.
type FileMeta struct {
	Key        string    `json:"key"`
	Filename   string    `json:"filename"`
	MIMEType   string    `json:"mime_type"`
	Size       int64     `json:"size"`
	Checksum   string    `json:"checksum,omitempty"`
	UploadedBy string    `json:"uploaded_by,omitempty"`
	UploadedAt time.Time `json:"uploaded_at,omitempty"`
	ModifiedAt time.Time `json:"modified_at,omitempty"`
}

// ErrNotFound is returned when an object does not exist.
var ErrNotFound = errors.New("storage: object not found")

// Backend abstracts where attachment blobs are stored. Keys are always
// forward-slash relative paths (e.g. "sites/acme/files/2026/08/photo.jpg").
type Backend interface {
	// Put stores r (of the given size) at key and returns the resulting metadata.
	Put(ctx context.Context, key string, r io.Reader, size int64, meta FileMeta) (*FileMeta, error)
	// EnsureBucket makes any backend-specific storage prerequisites available.
	// Local backends can no-op; S3-compatible backends should create missing buckets.
	EnsureBucket(ctx context.Context) error
	// Head returns metadata for key without transferring the body.
	Head(ctx context.Context, key string) (*FileMeta, error)
	// Open returns a reader for bytes [offset, offset+length). length < 0 means to EOF.
	Open(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error)
	// Delete removes the object at key. It is idempotent.
	Delete(ctx context.Context, key string) error
	// URL returns a public/signed URL for key, or "" when the protected serve endpoint
	// should be used instead.
	URL(ctx context.Context, key string) (string, error)
}

// Config configures a storage backend.
type Config struct {
	Backend         string // "local" (default) or "s3"
	LocalPath       string // local root directory (default ".")
	S3Endpoint      string // e.g. "https://minio.example.com" or "localhost:9000"
	S3Region        string // default "us-east-1"
	S3Bucket        string
	S3AccessKey     string
	S3SecretKey     string
	S3UseSSL        bool
	S3PublicBaseURL string        // optional public prefix for URL(); empty uses a presigned URL
	S3PresignTTL    time.Duration // presigned URL lifetime (default 15m)
}

// New resolves a backend from cfg.
func New(cfg Config) (Backend, error) {
	switch strings.ToLower(cfg.Backend) {
	case "", "local":
		return newLocalBackend(cfg), nil
	case "s3":
		return newS3Backend(cfg)
	default:
		return nil, fmt.Errorf("unknown storage backend %q", cfg.Backend)
	}
}

// baseName returns the final path segment of a forward-slash key.
func baseName(key string) string { return path.Base(key) }

// extOf returns the file extension (including the dot) of a forward-slash key.
func extOf(key string) string { return path.Ext(key) }
