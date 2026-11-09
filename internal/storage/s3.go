package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type s3Backend struct {
	endpoint   string // scheme + host, no trailing slash
	host       string // host[:port] used in signed headers
	region     string
	bucket     string
	creds      sigCreds
	client     *http.Client
	publicURL  string
	presignTTL time.Duration
}

func newS3Backend(cfg Config) (*s3Backend, error) {
	if cfg.S3Endpoint == "" {
		return nil, fmt.Errorf("s3 storage requires S3Endpoint")
	}
	if cfg.S3Bucket == "" {
		return nil, fmt.Errorf("s3 storage requires S3Bucket")
	}

	endpoint := cfg.S3Endpoint
	if !strings.HasPrefix(endpoint, "http://") && !strings.HasPrefix(endpoint, "https://") {
		if cfg.S3UseSSL {
			endpoint = "https://" + endpoint
		} else {
			endpoint = "http://" + endpoint
		}
	}
	endpoint = strings.TrimRight(endpoint, "/")

	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("s3 endpoint: %w", err)
	}

	region := cfg.S3Region
	if region == "" {
		region = "us-east-1"
	}

	presignTTL := cfg.S3PresignTTL
	if presignTTL <= 0 {
		presignTTL = 15 * time.Minute
	}

	return &s3Backend{
		endpoint:   endpoint,
		host:       u.Host,
		region:     region,
		bucket:     cfg.S3Bucket,
		creds:      sigCreds{accessKey: cfg.S3AccessKey, secretKey: cfg.S3SecretKey, region: region, service: "s3"},
		client:     &http.Client{Timeout: 60 * time.Second},
		publicURL:  strings.TrimRight(cfg.S3PublicBaseURL, "/"),
		presignTTL: presignTTL,
	}, nil
}

// bucketRequest builds a path-style bucket request: /{bucket}.
func (b *s3Backend) bucketRequest(ctx context.Context, method string) (*http.Request, error) {
	u, err := url.Parse(b.endpoint)
	if err != nil {
		return nil, err
	}
	u.Path = "/" + b.bucket
	return http.NewRequestWithContext(ctx, method, u.String(), nil)
}

// newRequest builds a path-style S3 request: /{bucket}/{key}.
func (b *s3Backend) newRequest(ctx context.Context, method, key string, body io.Reader) (*http.Request, error) {
	u, err := url.Parse(b.endpoint)
	if err != nil {
		return nil, err
	}
	u.Path = "/" + b.bucket + "/" + key
	return http.NewRequestWithContext(ctx, method, u.String(), body)
}

func (b *s3Backend) EnsureBucket(ctx context.Context) error {
	req, err := b.bucketRequest(ctx, http.MethodHead)
	if err != nil {
		return err
	}
	signV4(req, emptySHA256, b.creds, time.Now())

	resp, err := b.client.Do(req)
	if err == nil {
		defer resp.Body.Close()
		switch resp.StatusCode {
		case http.StatusOK, http.StatusNoContent:
			return nil
		case http.StatusNotFound:
			// Create the bucket below.
		case http.StatusForbidden:
			return fmt.Errorf("s3 head bucket forbidden: %s", resp.Status)
		default:
			if resp.StatusCode < 400 {
				return nil
			}
			return fmt.Errorf("s3 head bucket failed: %s", resp.Status)
		}
	} else {
		return err
	}

	req, err = b.bucketRequest(ctx, http.MethodPut)
	if err != nil {
		return err
	}
	signV4(req, emptySHA256, b.creds, time.Now())

	resp, err = b.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated, http.StatusNoContent, http.StatusConflict:
		return nil
	default:
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil
		}
		return fmt.Errorf("s3 create bucket failed: %s", resp.Status)
	}
}

func (b *s3Backend) Put(ctx context.Context, key string, r io.Reader, size int64, meta FileMeta) (*FileMeta, error) {
	body, err := io.ReadAll(io.LimitReader(r, size))
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(body)
	checksum := "sha256:" + hex.EncodeToString(sum[:])
	payloadHash := hex.EncodeToString(sum[:])

	req, err := b.newRequest(ctx, http.MethodPut, key, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.ContentLength = int64(len(body))
	if meta.MIMEType != "" {
		req.Header.Set("Content-Type", meta.MIMEType)
	}
	if meta.Filename != "" {
		req.Header.Set("X-Amz-Meta-Filename", meta.Filename)
	}
	req.Header.Set("X-Amz-Meta-Checksum", checksum)
	if meta.UploadedBy != "" {
		req.Header.Set("X-Amz-Meta-Uploaded-By", meta.UploadedBy)
	}
	if !meta.UploadedAt.IsZero() {
		req.Header.Set("X-Amz-Meta-Uploaded-At", meta.UploadedAt.UTC().Format(time.RFC3339Nano))
	}
	signV4(req, payloadHash, b.creds, time.Now())

	resp, err := b.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("s3 put failed: %s", resp.Status)
	}

	meta.Key = key
	meta.Size = int64(len(body))
	meta.Checksum = checksum
	if meta.Filename == "" {
		meta.Filename = baseName(key)
	}
	if meta.MIMEType == "" {
		meta.MIMEType = MimeByExt(extOf(key))
	}
	if meta.UploadedAt.IsZero() {
		meta.UploadedAt = time.Now().UTC()
	}
	meta.ModifiedAt = meta.UploadedAt
	return &meta, nil
}

func (b *s3Backend) Head(ctx context.Context, key string) (*FileMeta, error) {
	req, err := b.newRequest(ctx, http.MethodHead, key, nil)
	if err != nil {
		return nil, err
	}
	signV4(req, emptySHA256, b.creds, time.Now())

	resp, err := b.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("s3 head failed: %s", resp.Status)
	}

	meta := &FileMeta{
		Key:      key,
		Filename: baseName(key),
		MIMEType: MimeByExt(extOf(key)),
	}
	if v := resp.Header.Get("Content-Type"); v != "" {
		meta.MIMEType = v
	}
	if v := resp.Header.Get("Content-Length"); v != "" {
		var n int64
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			meta.Size = n
		}
	}
	if v := resp.Header.Get("Last-Modified"); v != "" {
		if t, err := http.ParseTime(v); err == nil {
			meta.ModifiedAt = t
		}
	}
	if v := resp.Header.Get("X-Amz-Meta-Filename"); v != "" {
		meta.Filename = v
	}
	if v := resp.Header.Get("X-Amz-Meta-Checksum"); v != "" {
		meta.Checksum = v
	}
	if v := resp.Header.Get("X-Amz-Meta-Uploaded-By"); v != "" {
		meta.UploadedBy = v
	}
	if v := resp.Header.Get("X-Amz-Meta-Uploaded-At"); v != "" {
		if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
			meta.UploadedAt = t
		}
	}
	return meta, nil
}

func (b *s3Backend) Open(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	req, err := b.newRequest(ctx, http.MethodGet, key, nil)
	if err != nil {
		return nil, err
	}
	if length >= 0 {
		end := offset + length - 1
		if end < offset {
			end = offset
		}
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, end))
	} else if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	signV4(req, emptySHA256, b.creds, time.Now())

	resp, err := b.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, ErrNotFound
	}
	if resp.StatusCode >= 400 {
		resp.Body.Close()
		return nil, fmt.Errorf("s3 get failed: %s", resp.Status)
	}
	return resp.Body, nil
}

func (b *s3Backend) Delete(ctx context.Context, key string) error {
	req, err := b.newRequest(ctx, http.MethodDelete, key, nil)
	if err != nil {
		return err
	}
	signV4(req, emptySHA256, b.creds, time.Now())

	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("s3 delete failed: %s", resp.Status)
	}
	return nil
}

func (b *s3Backend) URL(_ context.Context, key string) (string, error) {
	if b.publicURL != "" {
		return b.publicURL + "/" + key, nil
	}
	return b.presignGet(key, time.Now()), nil
}

// presignGet returns a SigV4 presigned GET URL valid for b.presignTTL.
func (b *s3Backend) presignGet(key string, now time.Time) string {
	now = now.UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	scope := fmt.Sprintf("%s/%s/s3/aws4_request", dateStamp, b.region)
	credential := b.creds.accessKey + "/" + scope

	params := map[string]string{
		"X-Amz-Algorithm":     "AWS4-HMAC-SHA256",
		"X-Amz-Credential":    credential,
		"X-Amz-Date":          amzDate,
		"X-Amz-Expires":       strconv.FormatInt(int64(b.presignTTL.Seconds()), 10),
		"X-Amz-SignedHeaders": "host",
	}
	query := presignedQuery(params)

	canonicalPath := canonicalURI("/" + b.bucket + "/" + key)
	canonicalRequest := strings.Join([]string{
		http.MethodGet,
		canonicalPath,
		query,
		"host:" + b.host,
		"host",
		"UNSIGNED-PAYLOAD",
	}, "\n")

	stringToSign := fmt.Sprintf("AWS4-HMAC-SHA256\n%s\n%s\n%s",
		amzDate, scope, sha256Hex([]byte(canonicalRequest)))
	signingKey := deriveSigningKey(b.creds.secretKey, dateStamp, b.region, "s3")
	signature := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	return b.endpoint + canonicalPath + "?" + query + "&X-Amz-Signature=" + signature
}
