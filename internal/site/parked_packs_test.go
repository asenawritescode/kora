package site

import (
	"os"
	"path/filepath"
	"testing"
)

// skipParkedPacks skips a test that needs a parked pack. The packs under
// config/_parked are not loaded until each is rebuilt on Item (#647).
func skipParkedPacks(t *testing.T, packs ...string) {
	t.Helper()
	if len(packs) == 0 {
		packs = []string{"batch-expiry", "erp-accounting", "erp-inventory", "erp-purchasing", "farm-produce", "job-cards", "manufacturing", "menu-tables", "erpsell"}
	}
	for _, pack := range packs {
		if _, err := os.Stat(filepath.Join("..", "..", "config", pack)); err != nil {
			t.Skipf("%s is parked in config/_parked until it is rebuilt on Item (#647)", pack)
		}
	}
}
