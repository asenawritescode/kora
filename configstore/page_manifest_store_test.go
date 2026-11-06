package configstore

import (
	"slices"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/doctype"
)

// A stored view reaches the page manifest readers in either config_json form:
// the historic component array or {resources, components} for views that
// declare resources. Both readers (list and by name) must accept both.
func TestPageManifestStore_ReadsBothViewConfigForms(t *testing.T) {
	const (
		listForm   = `[{"id":"table","type":"record_table","source_doctype":"Item"}]`
		objectForm = `{"resources":[{"id":"items","query":"published.query","params":{"owner_module_id":"inventory.core","query_name":"item_search"}}],` +
			`"components":[{"id":"grid","type":"product_grid","version":2,"context":{"till_session":"session.name"}}]}`
	)
	tests := []struct {
		name          string
		config        string
		source        string
		wantErr       string
		wantResources []string
		wantComponent string
	}{
		{name: "array form", config: listForm, source: "Item", wantResources: []string{"primary"}, wantComponent: "record_table"},
		{name: "object form keeps declared resources", config: objectForm, wantResources: []string{"items"}, wantComponent: "product_grid"},
		{name: "empty config", config: ""},
		{name: "empty array", config: "[]"},
		{name: "corrupt object", config: `{"resources":[`, wantErr: "decoding view config for page manifest"},
		{name: "wrong shape", config: `{"components":"nope"}`, wantErr: "decoding view config for page manifest"},
	}
	columns := []string{
		"name", "route", "type", "layout", "label", "module", "source_doctype",
		"public_enabled", "public_components", "public_allow_mutations", "config_json",
	}
	check := func(t *testing.T, tc int, manifest *doctype.PageManifest, err error) {
		t.Helper()
		want := tests[tc]
		if want.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), want.wantErr) {
				t.Fatalf("err = %v, want %q", err, want.wantErr)
			}
			return
		}
		if err != nil || manifest == nil {
			t.Fatalf("manifest = %v, err = %v", manifest, err)
		}
		var ids []string
		for _, resource := range manifest.Spec.Resources {
			ids = append(ids, resource.ID)
		}
		if !slices.Equal(ids, want.wantResources) {
			t.Fatalf("resource ids = %v, want %v", ids, want.wantResources)
		}
		var components []string
		for _, component := range manifest.Spec.Layout.Children {
			components = append(components, component.Component)
		}
		if want.wantComponent != "" && !slices.Equal(components, []string{want.wantComponent}) {
			t.Fatalf("components = %v, want [%s]", components, want.wantComponent)
		}
		if want.wantComponent == "product_grid" {
			child := manifest.Spec.Layout.Children[0]
			if child.Version != 2 || child.Props["context"].(map[string]string)["till_session"] != "session.name" {
				t.Fatalf("component lost version or context: %+v", child)
			}
		}
	}
	for i, tc := range tests {
		row := func() *sqlmock.Rows {
			return sqlmock.NewRows(columns).AddRow("Sell", "/sell", "register", "register", "", "pos", tc.source, 0, "", 0, tc.config)
		}
		t.Run(tc.name+"/list", func(t *testing.T) {
			s, mock := newViewStoreForTest(t)
			mock.ExpectQuery("SELECT .* FROM _kora_view WHERE site = \\? OR site = '' ORDER BY idx").WithArgs("site").WillReturnRows(row())
			manifests, err := s.LoadPageManifests("site")
			var manifest *doctype.PageManifest
			if len(manifests) == 1 {
				manifest = manifests[0]
			}
			check(t, i, manifest, err)
		})
		t.Run(tc.name+"/by name", func(t *testing.T) {
			s, mock := newViewStoreForTest(t)
			mock.ExpectQuery("SELECT .* FROM _kora_view WHERE name = \\? AND \\(site = \\? OR site = ''\\)").WithArgs("Sell", "site").WillReturnRows(row())
			manifest, err := s.LoadPageManifest("Sell", "site")
			check(t, i, manifest, err)
		})
	}
}
