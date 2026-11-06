//go:build integration

package engine

import (
	"os"
	"strings"
	"testing"
)

// The stored config_json is a native JSON column on MySQL and PostgreSQL; the
// page-manifest endpoints must read the register view back from it.
func TestPageManifestEndpointsServePackRegisterViewAcrossDialects(t *testing.T) {
	for _, tc := range []struct{ driver, env string }{
		{"mysql", "KORA_CONFIG_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "KORA_CONFIG_DISPOSABLE_POSTGRES_DSN"},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := strings.TrimSpace(os.Getenv(tc.env))
			if dsn == "" {
				t.Skipf("set %s to a disposable database", tc.env)
			}
			database, _ := isolatedDB(t, tc.driver, dsn)
			assertPageManifestEndpointsServeRegisterView(t, database, tc.driver)
		})
	}
}
