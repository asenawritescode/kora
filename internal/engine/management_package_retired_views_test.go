package engine

import (
	"testing"

	"github.com/asenawritescode/kora/internal/doctype"
)

// viewNamed is a component-free view on the given route; the upgrade only needs its identity.
func viewNamed(name, route, viewType string) *doctype.View {
	return &doctype.View{Name: name, Route: route, Type: viewType, Label: name, Module: "Selling"}
}

func TestDropRetiredViewsRemovesOnlyUnshippedRetiredNames(t *testing.T) {
	register := viewNamed("register", "/pos", "register")
	dashboard := viewNamed("owner_dashboard", "/dashboard", "dashboard")
	workshop := viewNamed("workshop", "/workshop", "register")
	for _, tc := range []struct {
		name     string
		views    []*doctype.View
		retired  []string
		shipped  []*doctype.View
		wantKeep []string
	}{
		{name: "no retires keeps everything", views: []*doctype.View{dashboard, register}, wantKeep: []string{"owner_dashboard", "register"}},
		{name: "retired view removed, others kept", views: []*doctype.View{dashboard, register, workshop}, retired: []string{"owner_dashboard"}, wantKeep: []string{"register", "workshop"}},
		{name: "retired but still shipped is kept", views: []*doctype.View{dashboard, register}, retired: []string{"owner_dashboard"}, shipped: []*doctype.View{dashboard}, wantKeep: []string{"owner_dashboard", "register"}},
		{name: "stored display name retired by snake case is removed", views: []*doctype.View{viewNamed("Owner Dashboard", "/dashboard", "dashboard"), register}, retired: []string{"owner_dashboard"}, wantKeep: []string{"register"}},
		{name: "retired name not installed is a no-op", views: []*doctype.View{register}, retired: []string{"owner_dashboard"}, wantKeep: []string{"register"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			incoming := &doctype.ConfigSnapshot{RetiredViews: tc.retired, Views: tc.shipped}
			got := dropRetiredViews(append([]*doctype.View(nil), tc.views...), incoming)
			names := make([]string, 0, len(got))
			for _, v := range got {
				names = append(names, v.Name)
			}
			if len(names) != len(tc.wantKeep) {
				t.Fatalf("kept %v, want %v", names, tc.wantKeep)
			}
			for i := range names {
				if names[i] != tc.wantKeep[i] {
					t.Fatalf("kept %v, want %v", names, tc.wantKeep)
				}
			}
		})
	}
}
