package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/internal/analytics"
	"github.com/asenawritescode/kora/internal/site"
)

func TestAnalyticsTenantWALDirIsStableAcrossRouteRenames(t *testing.T) {
	base := filepath.Join(t.TempDir(), "wal")
	first := AnalyticsTenantWALDir(base, "tenant-id-123")
	second := AnalyticsTenantWALDir(base, "tenant-id-123")
	if first != second || filepath.Dir(first) != base || !strings.HasPrefix(filepath.Base(first), "tenant-") {
		t.Fatalf("stable tenant WAL paths = %q and %q", first, second)
	}
	if AnalyticsTenantWALDir(base, "tenant-id-456") == first {
		t.Fatal("different stable tenant IDs share a WAL path")
	}
}

func TestMigrateAnalyticsWALDirectoriesMovesLegacyTenantDataAtomically(t *testing.T) {
	base := filepath.Join(t.TempDir(), "wal")
	const tenantID = "stable-tenant-1"
	legacy := AnalyticsSiteWALDir(base, "old-route.example")
	if err := os.MkdirAll(legacy, 0700); err != nil {
		t.Fatal("create legacy WAL directory:", err)
	}
	event := analytics.ChangeEvent{ID: "legacy-wal-event", Site: "old-route.example", Sequence: "42", Doctype: "Invoice", DocName: "INV-1"}
	record, err := json.Marshal(event)
	if err != nil {
		t.Fatal("encode legacy event:", err)
	}
	legacyFile := filepath.Join(legacy, "events.jsonl")
	if err := os.WriteFile(legacyFile, append(record, '\n'), 0600); err != nil {
		t.Fatal("write legacy event:", err)
	}

	owners := []analyticsWALNamespaceOwner{{tenantID: tenantID, siteNames: []string{"old-route.example"}}}
	if err := migrateAnalyticsWALDirectories(base, owners); err != nil {
		t.Fatalf("migrate legacy WAL directory: %v", err)
	}
	stable := AnalyticsTenantWALDir(base, tenantID)
	got, err := os.ReadFile(filepath.Join(stable, "events.jsonl"))
	if err != nil || string(got) != string(append(record, '\n')) {
		t.Fatalf("migrated WAL bytes = %q, %v; want original bytes", got, err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("legacy directory still exists after atomic move: %v", err)
	}
	if pending, err := analytics.WALHasPending(stable); err != nil || !pending {
		t.Fatalf("stable WAL pending = (%t, %v), want true", pending, err)
	}
	if err := migrateAnalyticsWALDirectories(base, owners); err != nil {
		t.Fatalf("repeat migration after restart: %v", err)
	}
}

func TestMigrateAnalyticsWALNamespacesResolvesDirectoryNameToStableTenantID(t *testing.T) {
	base := filepath.Join(t.TempDir(), "wal")
	const tenantID = "directory-stable-id"
	legacy := AnalyticsSiteWALDir(base, "old-alias.example")
	if err := os.MkdirAll(legacy, 0700); err != nil {
		t.Fatal("create legacy WAL directory:", err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "events.jsonl"), []byte("{\"id\":\"event\"}\n"), 0600); err != nil {
		t.Fatal("write legacy WAL:", err)
	}
	directory := &scannerActivationDirectory{
		tenantDirectoryStub: &tenantDirectoryStub{byID: site.DBSiteInfo{SiteID: tenantID, Name: "renamed.example"}},
		page:                []site.Descriptor{{SiteID: tenantID, Aliases: []string{"old-alias.example"}, Status: "active"}},
	}
	if err := migrateAnalyticsWALNamespaces(context.Background(), base, directory, "", nil); err != nil {
		t.Fatalf("migrate tenant WAL namespace from authoritative directory identity: %v", err)
	}
	if pending, err := analytics.WALHasPending(AnalyticsTenantWALDir(base, tenantID)); err != nil || !pending {
		t.Fatalf("stable tenant WAL pending = (%t, %v), want true", pending, err)
	}
}

func TestMigrateAnalyticsWALDirectoriesFailsClosedOnUnassignedPendingWAL(t *testing.T) {
	base := filepath.Join(t.TempDir(), "wal")
	legacy := AnalyticsSiteWALDir(base, "renamed-away.example")
	if err := os.MkdirAll(legacy, 0700); err != nil {
		t.Fatal("create legacy WAL directory:", err)
	}
	path := filepath.Join(legacy, "events.jsonl")
	const content = "{\"id\":\"unassigned\",\"site\":\"renamed-away.example\"}\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal("write unassigned event:", err)
	}

	err := migrateAnalyticsWALDirectories(base, []analyticsWALNamespaceOwner{{tenantID: "current-tenant", siteNames: []string{"current.example"}}})
	if err == nil || !strings.Contains(err.Error(), legacy) {
		t.Fatalf("migration error = %v, want path-specific unassigned-WAL failure", err)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil || string(got) != content {
		t.Fatalf("unassigned WAL after refusal = %q, %v; want preserved bytes", got, readErr)
	}
}

func TestMigrateAnalyticsWALDirectoriesRefusesToMergeTwoPendingStreams(t *testing.T) {
	base := filepath.Join(t.TempDir(), "wal")
	const tenantID = "merge-tenant"
	legacy := AnalyticsSiteWALDir(base, "merge.example")
	stable := AnalyticsTenantWALDir(base, tenantID)
	for dir, eventID := range map[string]string{legacy: "legacy", stable: "stable"} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal("create WAL directory:", err)
		}
		data := []byte("{\"id\":\"" + eventID + "\",\"site\":\"merge.example\"}\n")
		if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), data, 0600); err != nil {
			t.Fatal("write WAL event:", err)
		}
	}
	if err := migrateAnalyticsWALDirectories(base, []analyticsWALNamespaceOwner{{tenantID: tenantID, siteNames: []string{"merge.example"}}}); err == nil || !strings.Contains(err.Error(), "refusing to merge unordered logs") {
		t.Fatalf("migration error = %v, want explicit ordering conflict", err)
	}
	for _, path := range []string{filepath.Join(legacy, "events.jsonl"), filepath.Join(stable, "events.jsonl")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("WAL file %q lost after migration refusal: %v", path, err)
		}
	}
}
