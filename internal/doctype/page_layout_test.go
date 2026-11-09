package doctype

import "testing"

func TestPageManifestFromViewLayoutType(t *testing.T) {
	tests := []struct{ layout, want string }{
		{"register", "register"},
		{"two_panel", "two_panel"},
		{"three_panel", "three_panel"},
		{"grid", "grid"},
		{"", "single"},
		{"unknown", "single"},
	}
	for _, tc := range tests {
		t.Run(tc.layout, func(t *testing.T) {
			view := &View{Name: "Counter", Route: "/counter", Type: "register", Layout: tc.layout, Components: []ViewComponent{{ID: "status", Type: "cart_panel", Version: 2, Region: "status"}}}
			manifest := PageManifestFromView(view)
			if manifest.Spec.Layout.Type != tc.want {
				t.Fatalf("layout = %q, want %q", manifest.Spec.Layout.Type, tc.want)
			}
			if err := manifest.Validate(); err != nil {
				t.Fatalf("manifest invalid: %v", err)
			}
			if manifest.Spec.Layout.Children[0].Region != "status" {
				t.Fatalf("region = %q", manifest.Spec.Layout.Children[0].Region)
			}
		})
	}
}
