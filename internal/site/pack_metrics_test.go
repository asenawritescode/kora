package site

import (
	"path/filepath"
	"strings"
	"testing"
)

const posMetricCount = 4

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
