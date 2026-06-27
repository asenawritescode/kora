// Package webhook provides the webhook delivery system for Kora extensions.
package webhook

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Sign computes an HMAC-SHA256 signature for a webhook payload.
// Format: X-Kora-Signature: t={unix_timestamp},v1={hex_signature}
func Sign(secret string, body []byte, timestamp int64) string {
	payload := fmt.Sprintf("%d.%s", timestamp, string(body))
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))
	return fmt.Sprintf("t=%d,v1=%s", timestamp, hex.EncodeToString(mac.Sum(nil)))
}

// Verify checks a webhook signature against one or more secrets.
// Supports multi-secret verification for zero-downtime rotation.
// tolerance is the maximum age of the timestamp (default: 5 minutes).
func Verify(body []byte, header string, secrets []string, tolerance time.Duration) error {
	if header == "" {
		return fmt.Errorf("missing signature header")
	}

	// Parse header: "t=1765981391,v1=abc123,..."
	var timestamp int64
	var signatures []string
	for _, part := range strings.Split(header, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "t":
			timestamp, _ = strconv.ParseInt(kv[1], 10, 64)
		case "v1":
			signatures = append(signatures, kv[1])
		}
	}

	if timestamp == 0 {
		return fmt.Errorf("missing timestamp in signature")
	}

	// Replay protection: reject timestamps outside the tolerance window.
	if tolerance > 0 {
		age := time.Since(time.Unix(timestamp, 0))
		if age < 0 {
			age = -age
		}
		if age > tolerance {
			return fmt.Errorf("signature timestamp outside tolerance window (%v > %v)", age, tolerance)
		}
	}

	// Build expected signed payload.
	signedPayload := fmt.Sprintf("%d.%s", timestamp, string(body))

	// Verify against all active secrets (supports rotation).
	for _, secret := range secrets {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(signedPayload))
		expected := hex.EncodeToString(mac.Sum(nil))

		for _, sig := range signatures {
			// Constant-time comparison — non-negotiable.
			if subtle.ConstantTimeCompare([]byte(expected), []byte(sig)) == 1 {
				return nil
			}
		}
	}

	return fmt.Errorf("no valid signature found")
}

// GenerateSecret creates a new cryptographically random webhook signing secret.
func GenerateSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
