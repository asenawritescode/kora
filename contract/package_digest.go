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
		clean, err := packagePath(file.Path).normalize()
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

// packagePath is a file path inside a package artifact as authored.
type packagePath string

// normalize returns the cleaned path, rejecting absolute, empty, non-portable and traversing paths.
func (p packagePath) normalize() (string, error) {
	value := string(p)
	if value == "" || strings.ContainsRune(value, '\\') || strings.ContainsRune(value, 0) || strings.HasPrefix(value, "/") {
		return "", fmt.Errorf("absolute, empty, or non-portable path rejected")
	}
	clean := path.Clean(value)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || clean != value {
		return "", fmt.Errorf("path traversal or non-normalized path rejected")
	}
	return clean, nil
}

// ValidPackageFileType reports whether contentType is allowed at filePath.
func ValidPackageFileType(filePath, contentType string) bool {
	return validPackageFileType(filePath, contentType)
}

func validPackageFileType(filePath, contentType string) bool {
	ext := strings.ToLower(path.Ext(filePath))
	yml := ext == ".yaml" || ext == ".yml"
	switch contentType {
	case "doctype":
		return strings.HasPrefix(filePath, "doctypes/") && yml
	case "roles":
		return (filePath == "roles.yaml" || (strings.HasPrefix(filePath, "components/") && path.Base(filePath) == "roles.yaml")) && ext == ".yaml"
	case "permissions":
		return (filePath == "permissions.yaml" || (strings.HasPrefix(filePath, "components/") && path.Base(filePath) == "permissions.yaml")) && ext == ".yaml"
	case "workflow_definition":
		return strings.HasPrefix(filePath, "workflows/") && ext == ".json"
	case "view":
		return strings.HasPrefix(filePath, "views/") && yml
	case "report":
		return strings.HasPrefix(filePath, "reports/") && yml
	case "script":
		return strings.HasPrefix(filePath, "scripts/") && ext == ".js"
	case "query":
		return strings.HasPrefix(filePath, "queries/") && yml
	case "command":
		return strings.HasPrefix(filePath, "commands/") && yml
	case "print_format":
		return strings.HasPrefix(filePath, "print_formats/") && yml
	case "pack":
		return filePath == "pack.yaml" || (PackFileComponent(filePath) != "" && path.Base(filePath) == "pack.yaml" && path.Dir(filePath) == "components/"+PackFileComponent(filePath))
	default:
		return false
	}
}

// PackFileComponent returns the component namespace (16 lowercase hex chars)
// of a "components/<ns>/..." or "<dir>/components/<ns>/..." path, or "" for
// root files.
func PackFileComponent(filePath string) string {
	parts := strings.Split(filePath, "/")
	for i := 0; i+2 < len(parts); i++ {
		if parts[i] == "components" && packNamespace(parts[i+1]).valid() {
			return parts[i+1]
		}
	}
	return ""
}

// packNamespace is the 16-hex-digit namespace directory of a component inside a pack.
type packNamespace string

func (n packNamespace) valid() bool {
	if len(n) != 16 {
		return false
	}
	for _, c := range n {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// PackageFileType infers the content type of a package file from its path.
func PackageFileType(filePath string) (string, bool) {
	for _, t := range []string{"doctype", "roles", "permissions", "workflow_definition", "view", "report", "script", "query", "command", "print_format", "pack"} {
		if validPackageFileType(filePath, t) {
			return t, true
		}
	}
	return "", false
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
