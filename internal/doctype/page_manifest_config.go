package doctype

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// ParsePageManifestsDirectory loads native Page manifests from a config pack's
// pages directory. A missing directory is allowed for legacy packs.
func ParsePageManifestsDirectory(path string) ([]*PageManifest, error) {
	entries, err := os.ReadDir(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading page manifests directory: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	var manifests []*PageManifest
	seen := make(map[string]struct{})
	for _, entry := range entries {
		if entry.IsDir() || !(strings.HasSuffix(entry.Name(), ".yaml") || strings.HasSuffix(entry.Name(), ".yml")) {
			continue
		}
		file := filepath.Join(path, entry.Name())
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("reading page manifest %s: %w", entry.Name(), err)
		}
		var document any
		if err := yaml.Unmarshal(data, &document); err != nil {
			return nil, fmt.Errorf("parsing page manifest %s: %w", entry.Name(), err)
		}
		encoded, err := json.Marshal(document)
		if err != nil {
			return nil, fmt.Errorf("encoding page manifest %s: %w", entry.Name(), err)
		}
		var manifest PageManifest
		if err := json.Unmarshal(encoded, &manifest); err != nil {
			return nil, fmt.Errorf("decoding page manifest %s: %w", entry.Name(), err)
		}
		if err := manifest.Validate(); err != nil {
			return nil, fmt.Errorf("validating page manifest %s: %w", entry.Name(), err)
		}
		key := referenceKey(manifest.Metadata.Name)
		if _, exists := seen[key]; exists {
			return nil, fmt.Errorf("duplicate page manifest %q", manifest.Metadata.Name)
		}
		seen[key] = struct{}{}
		manifests = append(manifests, &manifest)
	}
	return manifests, nil
}
