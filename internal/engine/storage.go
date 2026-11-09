package engine

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/asenawritescode/kora/internal/site"
	"github.com/asenawritescode/kora/internal/storage"
)

// NewStorageResolver creates tenant storage backends and initializes their
// buckets under Engine ownership. The deployment-specific base config is
// supplied by the process configuration layer.
func NewStorageResolver(base storage.Config) func(*site.Config) (storage.Backend, error) {
	return newStorageResolver(base, storage.New)
}

func newStorageResolver(base storage.Config, create func(storage.Config) (storage.Backend, error)) func(*site.Config) (storage.Backend, error) {
	return func(siteConfig *site.Config) (storage.Backend, error) {
		config := base
		s3Configured := config.S3Endpoint != "" || config.S3Bucket != "" || config.S3AccessKey != "" || config.S3SecretKey != ""
		if siteConfig != nil && siteConfig.FileStorage != "" {
			config.Backend = siteConfig.FileStorage
		}
		if s3Configured {
			config.Backend = "s3"
		}
		if siteConfig != nil && config.Backend == "s3" && config.S3Bucket == "" {
			config.S3Bucket = siteConfig.StorageBucket
			if config.S3Bucket == "" {
				config.S3Bucket = site.BucketNameForSite(siteConfig.Hostname)
			}
		}
		backend, err := create(config)
		if err != nil {
			if closer, ok := backend.(io.Closer); ok {
				if closeErr := closer.Close(); closeErr != nil {
					err = errors.Join(err, fmt.Errorf("close storage backend after construction failure: %w", closeErr))
				}
			}
			return nil, err
		}
		if err := ensureStorageBucket(context.Background(), backend); err != nil {
			return nil, err
		}
		return backend, nil
	}
}

func ensureStorageBucket(ctx context.Context, backend interface{ EnsureBucket(context.Context) error }) error {
	if backend == nil {
		return errors.New("storage backend is required")
	}
	if err := backend.EnsureBucket(ctx); err != nil {
		wrapped := fmt.Errorf("ensure storage bucket: %w", err)
		if closer, ok := backend.(io.Closer); ok {
			if closeErr := closer.Close(); closeErr != nil {
				return errors.Join(wrapped, fmt.Errorf("close storage backend: %w", closeErr))
			}
		}
		return wrapped
	}
	return nil
}
