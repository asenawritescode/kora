package engine

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/asenawritescode/kora/internal/site"
	"github.com/asenawritescode/kora/internal/storage"
)

type startupStorageBackend struct {
	ensureErr error
	closeErr  error
	closed    bool
}

func (b *startupStorageBackend) Put(context.Context, string, io.Reader, int64, storage.FileMeta) (*storage.FileMeta, error) {
	return nil, nil
}
func (b *startupStorageBackend) EnsureBucket(context.Context) error { return b.ensureErr }
func (*startupStorageBackend) Head(context.Context, string) (*storage.FileMeta, error) {
	return nil, nil
}
func (*startupStorageBackend) Open(context.Context, string, int64, int64) (io.ReadCloser, error) {
	return nil, nil
}
func (*startupStorageBackend) Delete(context.Context, string) error { return nil }
func (*startupStorageBackend) URL(context.Context, string) (string, error) {
	return "", nil
}
func (b *startupStorageBackend) Close() error {
	b.closed = true
	return b.closeErr
}

func TestEnsureStorageBucketClosesBackendAndJoinsErrors(t *testing.T) {
	ensureErr := errors.New("bucket setup failed")
	closeErr := errors.New("storage transport close failed")
	backend := &startupStorageBackend{ensureErr: ensureErr, closeErr: closeErr}
	err := ensureStorageBucket(context.Background(), backend)
	if !backend.closed {
		t.Fatal("storage backend was not closed after bucket setup failed")
	}
	if !errors.Is(err, ensureErr) || !errors.Is(err, closeErr) {
		t.Fatalf("ensureStorageBucket error = %v, want setup and close errors", err)
	}
}

func TestEnsureStorageBucketLeavesBackendOpenOnSuccess(t *testing.T) {
	backend := &startupStorageBackend{}
	if err := ensureStorageBucket(context.Background(), backend); err != nil {
		t.Fatalf("ensureStorageBucket: %v", err)
	}
	if backend.closed {
		t.Fatal("storage backend closed after successful bucket setup")
	}
}

func TestStorageResolverAppliesTenantStorageAndBucket(t *testing.T) {
	backend := &startupStorageBackend{}
	var got storage.Config
	resolver := newStorageResolver(storage.Config{Backend: "local"}, func(config storage.Config) (storage.Backend, error) {
		got = config
		return backend, nil
	})
	_, err := resolver(&site.SiteConfig{Hostname: "Acme.Example", FileStorage: "s3"})
	if err != nil {
		t.Fatal("resolve storage:", err)
	}
	if got.Backend != "s3" || got.S3Bucket != site.BucketNameForSite("Acme.Example") {
		t.Fatalf("resolved storage config = %#v", got)
	}
	if backend.closed {
		t.Fatal("resolved backend was closed after successful bucket setup")
	}
}

func TestStorageResolverKeepsConfiguredS3Precedence(t *testing.T) {
	backend := &startupStorageBackend{}
	var got storage.Config
	resolver := newStorageResolver(storage.Config{Backend: "local", S3Endpoint: "https://objects.example", S3Bucket: "shared-bucket"}, func(config storage.Config) (storage.Backend, error) {
		got = config
		return backend, nil
	})
	if _, err := resolver(&site.SiteConfig{Hostname: "Acme.Example", FileStorage: "local"}); err != nil {
		t.Fatal("resolve storage:", err)
	}
	if got.Backend != "s3" || got.S3Bucket != "shared-bucket" {
		t.Fatalf("resolved storage config = %#v, want configured S3 backend and bucket", got)
	}
}

func TestStorageResolverClosesBackendReturnedWithCreateError(t *testing.T) {
	createErr := errors.New("backend construction failed after allocation")
	backend := &startupStorageBackend{}
	resolver := newStorageResolver(storage.Config{}, func(storage.Config) (storage.Backend, error) {
		return backend, createErr
	})
	_, err := resolver(&site.SiteConfig{Hostname: "tenant.example"})
	if !errors.Is(err, createErr) {
		t.Fatalf("resolve error = %v, want backend construction error", err)
	}
	if !backend.closed {
		t.Fatal("partially constructed backend was not closed")
	}
}
