package kernel

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/asenawritescode/kora/contract"
)

// expectedVersionFrom reads the optimistic-concurrency token from the
// envelope-level convention: Context.CausationID prefixed "expected:".
// A dedicated envelope field lands with SPEC-003's canonical envelope work;
// absence of the token preserves backward compatibility (no guard).
func expectedVersionFrom(op Operation) string {
	if strings.TrimSpace(op.Context.ExpectedVersion) != "" {
		return strings.TrimSpace(op.Context.ExpectedVersion)
	}
	if strings.HasPrefix(op.Context.CausationID, "expected:") {
		return strings.TrimPrefix(op.Context.CausationID, "expected:")
	}
	return ""
}

// CanonicalVersion renders a document version token (the `modified`
// timestamp) in a stable string form so callers can echo it back as
// expected_version regardless of driver time parsing.
func CanonicalVersion(v any) string {
	switch t := v.(type) {
	case time.Time:
		return t.UTC().Format(time.RFC3339Nano)
	case string:
		if parsed, err := parseVersionTimestamp(t); err == nil {
			return parsed.UTC().Format(time.RFC3339Nano)
		}
		return t
	case []byte:
		return CanonicalVersion(string(t))
	default:
		return fmt.Sprintf("%v", v)
	}
}

func parseVersionTimestamp(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	for _, layout := range []string{
		time.RFC3339Nano,
		"2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02T15:04:05.999999999",
	} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid version timestamp %q", value)
}

// CanonicalDocHash renders a document's field map as sorted-key JSON and
// returns the sha256 hex digest — the audit before/after state token
// (KERNEL-009). Hash input is deterministic: sorted keys, JSON-encoded values.
func CanonicalDocHash(fields map[string]any) string {
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	sb.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			sb.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		vb, _ := json.Marshal(fields[k])
		sb.Write(kb)
		sb.WriteByte(':')
		sb.Write(vb)
	}
	sb.WriteByte('}')
	sum := sha256.Sum256([]byte(sb.String()))
	return hex.EncodeToString(sum[:])
}

func payloadHash(raw json.RawMessage) string {
	canonical, err := canonicalJSON(raw)
	if err == nil {
		raw = canonical
	}
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}

func rawPayloadHash(raw json.RawMessage) string {
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}

func canonicalJSON(raw json.RawMessage) ([]byte, error) {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, errors.New("multiple JSON values")
		}
		return nil, err
	}
	return json.Marshal(value)
}

func resultHash(r contract.CommandResult) string {
	raw, _ := json.Marshal(struct {
		Status contract.Status `json:"status"`
		Data   json.RawMessage `json:"data,omitempty"`
		Error  *contract.Error `json:"error,omitempty"`
	}{r.Status, r.Data, r.Error})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
