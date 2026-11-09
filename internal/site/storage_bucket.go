package site

import (
	"os"
	"strings"
	"unicode"
)

// DefaultFileStorageFromEnv resolves the default storage backend for new sites.
// It stays backend-agnostic: operators can switch between any S3-compatible
// service by setting KORA_STORAGE_BACKEND=s3 plus the standard S3 env vars.
func DefaultFileStorageFromEnv() string {
	if v := os.Getenv("KORA_STORAGE_BACKEND"); v != "" {
		return v
	}
	if os.Getenv("KORA_STORAGE_S3_ENDPOINT") != "" {
		return "s3"
	}
	return "local"
}

// BucketNameForSite derives a stable bucket name for a site.
// The output is intentionally conservative so it works across S3-compatible
// backends and can be used for billing / quota partitioning.
func BucketNameForSite(siteName string) string {
	name := strings.ToLower(strings.TrimSpace(siteName))
	if name == "" {
		return "kora-site"
	}

	var b strings.Builder
	b.Grow(len(name))
	lastHyphen := false
	for _, r := range name {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r):
			b.WriteRune(r)
			lastHyphen = false
		case r == '.' || r == '-':
			if !lastHyphen {
				b.WriteRune(r)
				lastHyphen = r == '-'
			}
		default:
			if !lastHyphen {
				b.WriteRune('-')
				lastHyphen = true
			}
		}
	}

	out := strings.Trim(b.String(), ".-")
	if len(out) < 3 {
		out = "kora-" + out
	}
	if len(out) > 63 {
		out = strings.Trim(out[:63], ".-")
	}
	if out == "" {
		out = "kora-site"
	}
	return out
}
