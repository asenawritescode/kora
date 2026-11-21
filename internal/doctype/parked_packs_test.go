package doctype

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
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

// TestParkedPacksAreNotLoaded: the parked packs sit only under config/_parked,
// no maintained pack directory is named after them, and the walkers that visit
// config/ skip the parked directory.
func TestParkedPacksAreNotLoaded(t *testing.T) {
	config := filepath.Join("..", "..", "config")
	entries, err := os.ReadDir(filepath.Join(config, "_parked"))
	if err != nil {
		t.Fatal(err)
	}
	var parked []string
	for _, entry := range entries {
		if entry.IsDir() {
			parked = append(parked, entry.Name())
		}
	}
	want := []string{"batch-expiry", "erp-accounting", "erp-inventory", "erp-purchasing", "erpsell", "farm-produce", "job-cards", "manufacturing", "menu-tables"}
	if !slices.Equal(parked, want) {
		t.Fatalf("config/_parked holds %v, want %v", parked, want)
	}
	top, err := os.ReadDir(config)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range top {
		if strings.HasPrefix(entry.Name(), "_") && entry.Name() != "_parked" {
			t.Errorf("unexpected underscore directory %s in config/", entry.Name())
		}
	}
	for _, name := range want {
		if _, err := os.Stat(filepath.Join(config, name)); !os.IsNotExist(err) {
			t.Errorf("parked pack %s is also loadable as config/%s", name, name)
		}
	}
}
