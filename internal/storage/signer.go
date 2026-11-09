package storage

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

type sigCreds struct {
	accessKey string
	secretKey string
	region    string
	service   string
}

// signV4 signs req in place with AWS Signature Version 4. It sets the
// Authorization, X-Amz-Date, and X-Amz-Content-Sha256 headers. payloadHash is the
// hex-encoded SHA-256 of the request body (emptySHA256 for body-less requests).
func signV4(req *http.Request, payloadHash string, creds sigCreds, now time.Time) {
	now = now.UTC()
	amzDate := now.Format("20060102T150405Z")
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)

	scope := fmt.Sprintf("%s/%s/%s/aws4_request", now.Format("20060102"), creds.region, creds.service)
	canonicalRequest := buildCanonicalRequest(req)
	stringToSign := fmt.Sprintf("AWS4-HMAC-SHA256\n%s\n%s\n%s",
		amzDate, scope, sha256Hex([]byte(canonicalRequest)))

	signingKey := deriveSigningKey(creds.secretKey, now.Format("20060102"), creds.region, creds.service)
	signature := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s,SignedHeaders=%s,Signature=%s",
		creds.accessKey, scope, signedHeaderNames(req), signature,
	))
}

func buildCanonicalRequest(req *http.Request) string {
	canonicalHeaders, signedHeaders := canonicalHeaders(req)
	return strings.Join([]string{
		req.Method,
		canonicalURI(req.URL.Path),
		canonicalQuery(req.URL.Query()),
		canonicalHeaders,
		signedHeaders,
		req.Header.Get("X-Amz-Content-Sha256"),
	}, "\n")
}

func canonicalHeaders(req *http.Request) (string, string) {
	headers := make(map[string]string, len(req.Header)+1)
	for name, vals := range req.Header {
		key := strings.ToLower(name)
		headers[key] = strings.TrimSpace(strings.Join(vals, ","))
	}
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	headers["host"] = host

	keys := make([]string, 0, len(headers))
	for k := range headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var canon strings.Builder
	for _, k := range keys {
		canon.WriteString(k)
		canon.WriteByte(':')
		canon.WriteString(headers[k])
		canon.WriteByte('\n')
	}
	return canon.String(), strings.Join(keys, ";")
}

func signedHeaderNames(req *http.Request) string {
	keys := make([]string, 0, len(req.Header)+1)
	for k := range req.Header {
		keys = append(keys, strings.ToLower(k))
	}
	keys = append(keys, "host")
	sort.Strings(keys)
	return strings.Join(keys, ";")
}

func canonicalURI(decodedPath string) string {
	return uriEncode(decodedPath, false)
}

func canonicalQuery(vals map[string][]string) string {
	if len(vals) == 0 {
		return ""
	}
	keys := make([]string, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		v := vals[k]
		if len(v) == 0 {
			parts = append(parts, uriEncode(k, true)+"=")
			continue
		}
		for _, item := range v {
			parts = append(parts, uriEncode(k, true)+"="+uriEncode(item, true))
		}
	}
	return strings.Join(parts, "&")
}

// uriEncode percent-encodes a string per AWS SigV4 rules: unreserved characters
// (A-Za-z0-9-._~) are kept, '/' is kept when encodeSlash is false.
func uriEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '.' || c == '_' || c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// presignedQuery builds the sorted, encoded query string used both in the
// canonical request and the final presigned URL.
func presignedQuery(params map[string]string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, uriEncode(k, true)+"="+uriEncode(params[k], true))
	}
	return strings.Join(parts, "&")
}

func deriveSigningKey(secret, date, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), []byte(date))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	return hmacSHA256(kService, []byte("aws4_request"))
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func sha256Hex(data []byte) string {
	s := sha256.Sum256(data)
	return hex.EncodeToString(s[:])
}
