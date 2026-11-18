package site

import (
	"os"
	"path/filepath"
	"testing"
)

// skipParkedERPLayers skips a test that needs the ERP layers. pos-base,
// erp-purchasing, erp-accounting and erp-inventory are parked under
// config/_parked until each is rebuilt on Item (#647).
func skipParkedERPLayers(t *testing.T) {
	t.Helper()
	for _, pack := range []string{"pos-base", "erp-purchasing", "erp-accounting", "erp-inventory"} {
		if _, err := os.Stat(filepath.Join("..", "..", "config", pack)); err != nil {
			t.Skipf("%s is parked in config/_parked until it is rebuilt on Item (#647)", pack)
		}
	}
}
