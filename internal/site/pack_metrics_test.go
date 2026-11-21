package site

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/internal/metrics"
)

const posMetricCount = 5

// Directory import: parsePackSnapshot reads metrics/ next to reports.
func TestDirectoryImportCarriesPOSMetrics(t *testing.T) {
	snap, err := parsePackSnapshot(filepath.Join("..", "..", "config", "pos"))
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Metrics) != posMetricCount {
		t.Fatalf("directory import carried %d metrics; want %d", len(snap.Metrics), posMetricCount)
	}
}

// Management artifacts and Cloud installs parse package files with ParseConfigPack.
func TestParseConfigPackCarriesPOSMetrics(t *testing.T) {
	files := readPackFiles(t, filepath.Join("..", "..", "config", "pos"), "")
	snap, _, err := ParseConfigPack("pos", files)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Metrics) != posMetricCount {
		t.Fatalf("package parse carried %d metrics; want %d", len(snap.Metrics), posMetricCount)
	}
}

// File upload imports a single pack through parsePackPart; a bad metric file must fail the upload.
func TestUploadedPackRejectsBadMetricFile(t *testing.T) {
	files := readPackFiles(t, filepath.Join("..", "..", "config", "pos"), "")
	for i := range files {
		if files[i].ContentType == "metric" && strings.Contains(files[i].Content, "measure:") {
			files[i].Content = strings.Replace(files[i].Content, "measure:", "measur:", 1)
			break
		}
	}
	if _, _, err := parsePackPart("pos", files); err == nil || !strings.Contains(err.Error(), "measur") {
		t.Fatalf("upload with bad metric = %v; want unknown key error", err)
	}
	good := readPackFiles(t, filepath.Join("..", "..", "config", "pos"), "")
	snap, _, err := parsePackPart("pos", good)
	if err != nil || len(snap.Metrics) != posMetricCount {
		t.Fatalf("upload carried %d metrics, %v; want %d", len(snap.Metrics), err, posMetricCount)
	}
}

// decodeMetrics turns the snapshot documents back into definitions, sorted by name.
func decodeMetrics(t *testing.T, raw []json.RawMessage) []metrics.Metric {
	t.Helper()
	out := make([]metrics.Metric, 0, len(raw))
	for _, r := range raw {
		var m metrics.Metric
		if err := json.Unmarshal(r, &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// The directory, package and upload install paths decode to the same metric definitions.
func TestMetricDefinitionsAgreeAcrossInstallPaths(t *testing.T) {
	packDir := filepath.Join("..", "..", "config", "pos")
	dir, err := parsePackSnapshot(packDir)
	if err != nil {
		t.Fatal(err)
	}
	files := readPackFiles(t, packDir, "")
	pkg, _, err := ParseConfigPack("pos", files)
	if err != nil {
		t.Fatal(err)
	}
	upload, _, err := parsePackPart("pos", files)
	if err != nil {
		t.Fatal(err)
	}
	want := decodeMetrics(t, dir.Metrics)
	if len(want) != posMetricCount {
		t.Fatalf("directory install carried %d metrics; want %d", len(want), posMetricCount)
	}
	for name, raw := range map[string][]json.RawMessage{"package": pkg.Metrics, "upload": upload.Metrics} {
		if got := decodeMetrics(t, raw); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s install metrics = %+v; want %+v", name, got, want)
		}
	}
}
