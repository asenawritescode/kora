//go:build integration

package engine

import (
	"context"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/internal/configstore"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/site"
)

// An upgrade removes a view only when the new release lists it in pack.yaml retires.
// Without the list, the view survives (documented current behaviour).
func TestUpgradeRemovesOnlyViewsTheReleaseRetires(t *testing.T) {
	for _, tc := range []struct {
		name    string
		retires []string
		want    []string
	}{
		{name: "retired view removed, other views kept", retires: []string{"owner_dashboard"}, want: []string{"register", "workshop"}},
		{name: "without retires the view survives", want: []string{"owner_dashboard", "register", "workshop"}},
	} {
		for _, dc := range []struct{ driver, env string }{
			{"mysql", "KORA_MANAGEMENT_DISPOSABLE_MYSQL_DSN"},
			{"postgres", "KORA_MANAGEMENT_DISPOSABLE_POSTGRES_DSN"},
		} {
			t.Run(tc.name+"/"+dc.driver, func(t *testing.T) {
				dsn := strings.TrimSpace(os.Getenv(dc.env))
				if dsn == "" {
					t.Skipf("set %s to a disposable %s server", dc.env, dc.driver)
				}
				conn, info, cleanup := openManagementPackageTestDatabase(t, dc.driver, dsn)
				defer cleanup()
				dialect := db.Resolve(dc.driver)
				if err := site.BootstrapSystemTables(context.Background(), conn, dialect); err != nil {
					t.Fatal(err)
				}
				installed := configSnapshot(t, nil, nil, nil)
				installed.Views = []*doctype.View{
					viewNamed("owner_dashboard", "/dashboard", "dashboard"),
					viewNamed("register", "/pos", "register"),
					viewNamed("workshop", "/workshop", "register"), // another module's view
				}
				if err := site.ImportConfigFromSnapshotContext(t.Context(), conn, doctype.NewRegistry(), info.DBName, "r1.example", "r1.example", "install", installed, dialect, nil); err != nil {
					t.Fatal(err)
				}
				release := configSnapshot(t, nil, nil, nil)
				release.Views = []*doctype.View{viewNamed("register", "/pos", "register")}
				release.RetiredViews = tc.retires
				merged, err := mergeManagementPackageSnapshot(installed, release, packageAuthority{})
				if err != nil {
					t.Fatal(err)
				}
				if err = site.ImportConfigFromSnapshotContext(t.Context(), conn, doctype.NewRegistry(), info.DBName, "r1.example", "r1.example", "upgrade", merged, dialect, nil); err != nil {
					t.Fatal(err)
				}
				got, _, err := configstore.NewStore(conn, dialect).LoadActiveSnapshot("r1.example")
				if err != nil {
					t.Fatal(err)
				}
				names := make([]string, 0, len(got.Views))
				for _, v := range got.Views {
					names = append(names, v.Name)
				}
				sort.Strings(names)
				if strings.Join(names, ",") != strings.Join(tc.want, ",") {
					t.Fatalf("views after upgrade = %v, want %v", names, tc.want)
				}
			})
		}
	}
}
