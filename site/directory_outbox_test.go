package site

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDirectoryChangeWirePayloadOmitsTenantCredentials(t *testing.T) {
	change := SiteDirectoryChange{
		Cursor: 19,
		ID:     "01J00000000000000000000000",
		Descriptor: DescriptorFromDBSiteInfo(DBSiteInfo{
			SiteID:         "site_0123456789abcdef0123456789abcdef",
			Name:           "pos.example.test",
			Domains:        []string{"pos.example.test", "shop.example.test"},
			Status:         "active",
			ConfigRevision: 4,
			DBType:         "mysql",
			DBHost:         "mysql.internal",
			DBName:         "tenant_database_sentinel",
			DBUser:         "tenant_user_sentinel",
			DBPassword:     "tenant_password_sentinel",
		}),
	}
	encoded, err := json.Marshal(change)
	if err != nil {
		t.Fatal("marshal directory change:", err)
	}
	for _, sentinel := range []string{"tenant_database_sentinel", "tenant_user_sentinel", "tenant_password_sentinel", "db_password"} {
		if strings.Contains(string(encoded), sentinel) {
			t.Fatalf("directory change payload exposed %q: %s", sentinel, encoded)
		}
	}
	if !strings.Contains(string(encoded), change.Descriptor.SiteID) || !strings.Contains(string(encoded), `"config_revision":4`) {
		t.Fatalf("directory change omitted canonical identity or revision: %s", encoded)
	}
}

func TestSiteBelongsToRuntimeCell(t *testing.T) {
	for _, test := range []struct {
		name, siteCell, processCell string
		want                        bool
	}{
		{"unconfigured process preserves single-node behavior", "cell-a", "", true},
		{"legacy site belongs to default", "", "default", true},
		{"legacy site is excluded from other cell", "", "cell-a", false},
		{"matching explicit cell", "cell-a", "cell-a", true},
		{"different explicit cell", "cell-a", "cell-b", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := SiteBelongsToRuntimeCell(test.siteCell, test.processCell); got != test.want {
				t.Fatalf("SiteBelongsToRuntimeCell(%q, %q) = %v, want %v", test.siteCell, test.processCell, got, test.want)
			}
		})
	}
}

func TestNormalizeRuntimeCellID(t *testing.T) {
	if got, err := normalizeRuntimeCellID("  CELL-A.prod_1 "); err != nil || got != "cell-a.prod_1" {
		t.Fatalf("normalize valid cell = %q, %v", got, err)
	}
	for _, invalid := range []string{"a/b", "cell name", strings.Repeat("a", 81)} {
		if _, err := normalizeRuntimeCellID(invalid); err == nil {
			t.Errorf("normalizeRuntimeCellID(%q) succeeded; want validation error", invalid)
		}
	}
}
