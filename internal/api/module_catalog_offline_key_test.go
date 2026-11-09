package api

import "testing"

func TestCatalogOfflineFactKey(t *testing.T) {
	for _, tc := range []struct {
		name     string
		key      string
		eligible bool
		want     string
	}{
		{"eligible keeps key", "sku", true, "sku"},
		{"ineligible drops key", "sku", false, ""},
		{"ineligible no key", "", false, ""},
	} {
		if got := catalogOfflineFactKey(tc.key, tc.eligible); got != tc.want {
			t.Fatalf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}
