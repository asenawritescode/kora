package contract

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"path"
	"sort"
	"strings"
	"unicode/utf16"
)

const packageHashScheme = "kora-pack-v1"

// CanonicalManifestJSON emits RFC 8785 compatible JSON for the v1 marketplace
// manifest shape. V1 numeric fields are integers in the interoperable range.
func CanonicalManifestJSON(manifest any) ([]byte, error) {
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return nil, fmt.Errorf("marshal pack manifest: %w", err)
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("decode pack manifest: %w", err)
	}
	var out bytes.Buffer
	if err := appendCanonicalPackageJSON(&out, value); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func appendCanonicalPackageJSON(out *bytes.Buffer, value any) error {
	switch v := value.(type) {
	case nil:
		out.WriteString("null")
	case bool:
		if v {
			out.WriteString("true")
		} else {
			out.WriteString("false")
		}
	case string:
		writeCanonicalJSONString(out, v)
	case json.Number:
		integer := new(big.Int)
		if _, ok := integer.SetString(string(v), 10); !ok || !integer.IsInt64() || integer.Int64() > 1<<53-1 || integer.Int64() < -(1<<53-1) {
			return fmt.Errorf("%s only permits safe integer JSON numbers: %q", packageHashScheme, v)
		}
		out.WriteString(integer.String())
	case []any:
		out.WriteByte('[')
		for i, item := range v {
			if i != 0 {
				out.WriteByte(',')
			}
			if err := appendCanonicalPackageJSON(out, item); err != nil {
				return err
			}
		}
		out.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Slice(keys, func(i, j int) bool { return packageUTF16Less(keys[i], keys[j]) })
		out.WriteByte('{')
		for i, key := range keys {
			if i != 0 {
				out.WriteByte(',')
			}
			writeCanonicalJSONString(out, key)
			out.WriteByte(':')
			if err := appendCanonicalPackageJSON(out, v[key]); err != nil {
				return err
			}
		}
		out.WriteByte('}')
	default:
		return fmt.Errorf("unsupported canonical JSON value %T", value)
	}
	return nil
}

func packageUTF16Less(a, b string) bool {
	left, right := utf16.Encode([]rune(a)), utf16.Encode([]rune(b))
	for i := 0; i < len(left) && i < len(right); i++ {
		if left[i] != right[i] {
			return left[i] < right[i]
		}
	}
	return len(left) < len(right)
}

func writeCanonicalJSONString(out *bytes.Buffer, value string) {
	encoded, _ := json.Marshal(value)
	encoded = bytes.ReplaceAll(encoded, []byte(`\u003c`), []byte("<"))
	encoded = bytes.ReplaceAll(encoded, []byte(`\u003e`), []byte(">"))
	encoded = bytes.ReplaceAll(encoded, []byte(`\u0026`), []byte("&"))
	out.Write(encoded)
}

// PackDigest computes the kora-pack-v1 digest over canonical manifest bytes
// and sorted, length-prefixed files. It enforces transport-level path, file
// type, and byte limits; Engine remains responsible for parsing file content.
func PackDigest(canonicalManifest []byte, files []PackFile) (string, error) {
	var manifest any
	if err := json.Unmarshal(canonicalManifest, &manifest); err != nil {
		return "", fmt.Errorf("invalid canonical manifest: %w", err)
	}
	canonical, err := CanonicalManifestJSON(manifest)
	if err != nil {
		return "", err
	}
	if !bytes.Equal(canonical, canonicalManifest) {
		return "", fmt.Errorf("manifest bytes are not canonical JSON")
	}
	if len(canonicalManifest) == 0 {
		return "", fmt.Errorf("manifest is empty")
	}
	cleanFiles := make([]PackFile, 0, len(files))
	seen := map[string]bool{}
	total := 0
	for _, file := range files {
		clean, err := normalizePackagePath(file.Path)
		if err != nil {
			return "", fmt.Errorf("pack file %q: %w", file.Path, err)
		}
		if seen[clean] {
			return "", fmt.Errorf("duplicate normalized pack path %q", clean)
		}
		seen[clean] = true
		if !validPackageFileType(clean, file.ContentType) {
			return "", fmt.Errorf("pack file %q has unsupported content type %q", clean, file.ContentType)
		}
		if len(file.Content) > 256*1024 {
			return "", fmt.Errorf("pack file %q exceeds 256 KB", clean)
		}
		total += len(file.Content)
		file.Path = clean
		cleanFiles = append(cleanFiles, file)
	}
	if total > 2*1024*1024 {
		return "", fmt.Errorf("pack exceeds 2 MB")
	}
	sort.Slice(cleanFiles, func(i, j int) bool { return cleanFiles[i].Path < cleanFiles[j].Path })
	h := sha256.New()
	_, _ = h.Write([]byte(packageHashScheme + "\x00"))
	writePackageLength(h, uint64(len(canonicalManifest)))
	_, _ = h.Write(canonicalManifest)
	writePackageLength(h, uint64(len(cleanFiles)))
	for _, file := range cleanFiles {
		for _, part := range []string{file.Path, file.ContentType, file.Content} {
			value := []byte(part)
			writePackageLength(h, uint64(len(value)))
			_, _ = h.Write(value)
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func normalizePackagePath(value string) (string, error) {
	if value == "" || strings.ContainsRune(value, '\\') || strings.ContainsRune(value, 0) || strings.HasPrefix(value, "/") {
		return "", fmt.Errorf("absolute, empty, or non-portable path rejected")
	}
	clean := path.Clean(value)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || clean != value {
		return "", fmt.Errorf("path traversal or non-normalized path rejected")
	}
	return clean, nil
}

func validPackageFileType(filePath, contentType string) bool {
	ext := strings.ToLower(path.Ext(filePath))
	switch contentType {
	case "doctype":
		return strings.HasPrefix(filePath, "doctypes/") && (ext == ".yaml" || ext == ".yml")
	case "roles":
		return filePath == "roles.yaml" && ext == ".yaml"
	case "permissions":
		return filePath == "permissions.yaml" && ext == ".yaml"
	case "workflow":
		return strings.Contains(filePath, "workflow") && (ext == ".yaml" || ext == ".yml")
	case "view":
		return strings.HasPrefix(filePath, "views/") && (ext == ".yaml" || ext == ".yml")
	case "script":
		return strings.HasPrefix(filePath, "scripts/") && ext == ".js"
	default:
		return false
	}
}

func writePackageLength(w interface{ Write([]byte) (int, error) }, value uint64) {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], value)
	_, _ = w.Write(buf[:])
}

// ManifestDigest returns SHA-256 over canonical manifest bytes.
func ManifestDigest(canonical []byte) string {
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:])
}

// ParsePackDigest verifies the stored lowercase digest shape.
func ParsePackDigest(value string) error {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return fmt.Errorf("invalid %s digest", packageHashScheme)
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != sha256.Size {
		return fmt.Errorf("invalid %s digest", packageHashScheme)
	}
	return nil
}
