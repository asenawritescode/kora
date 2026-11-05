//go:build integration

package engine

import (
	"os"
	"strings"
	"testing"
)

func TestPOSPackTenantServicesInstallAcrossDialects(t *testing.T) {
	for _, tc := range []struct{ driver, env string }{
		{"mysql", "KORA_CONFIG_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "KORA_CONFIG_DISPOSABLE_POSTGRES_DSN"},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := strings.TrimSpace(os.Getenv(tc.env))
			if dsn == "" {
				t.Skipf("set %s to a disposable database", tc.env)
			}
			database, name := isolatedDB(t, tc.driver, dsn)
			installPackTenant(t, tc.driver, database, name)
		})
	}
}
