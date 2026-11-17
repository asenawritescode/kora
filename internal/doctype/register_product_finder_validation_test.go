package doctype

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/contract"
)

const productFinderRegisterViewYAML = `
name: Till
route: /sell
type: register
layout: register
resources:
  - id: items
    query: published.query
    params: {owner_module_id: shop.core, query_name: item_search, input: {}}
  - id: rank
    query: published.query
    params: {owner_module_id: shop.core, query_name: item_rank}
  - id: categories
    query: published.query
    params: {owner_module_id: shop.core, query_name: item_categories}
components:
  - id: finder
    type: product_finder
    version: 2
    region: main
    position: 0
    resource_bindings: {items: items, rank: rank, categories: categories}
    bindings:
      title: item_name
      price: price
      local_name: local_name
      shelf_code: shelf_code
      barcode: barcode
      subtitle: uom
      image: image
      category: category
      favourite: favourite_position
      rank_key: name
      category_label_field: category_name
`

// bindStockRole adds the optional stock resource role with all three stock bindings.
func bindStockRole(view *View) {
	view.Resources = append(view.Resources, PageResource{ID: "stock", Query: "published.query", Params: map[string]any{
		"owner_module_id": "shop.core", "query_name": "item_stock", "input": map[string]any{},
	}})
	view.Components[0].ResourceBindings["stock"] = "stock"
	view.Components[0].Bindings["stock_item"] = "item"
	view.Components[0].Bindings["stock_quantity"] = "quantity"
	view.Components[0].Bindings["stock_location"] = "location"
}

func productFinderModule() contract.ModuleDescriptor {
	module := publishedRegisterModule(true, true)
	module.Queries = append(module.Queries,
		contract.PublishedQuery{
			OwnerModuleID: "shop.core", Name: "item_search",
			AllowedFields: []string{"name", "item_name", "local_name", "shelf_code", "barcode", "uom", "price", "category", "image", "favourite_position"},
		},
		contract.PublishedQuery{OwnerModuleID: "shop.core", Name: "item_rank", AllowedFields: []string{"name"}},
		contract.PublishedQuery{OwnerModuleID: "shop.core", Name: "item_categories", AllowedFields: []string{"name", "category_name", "display_order"}},
		contract.PublishedQuery{OwnerModuleID: "shop.core", Name: "item_stock", AllowedFields: []string{"name", "item", "location", "quantity"}},
	)
	module.Schemas = append(module.Schemas, contract.ModuleSchemaDocument{Ref: contract.SchemaRef{ID: "shop.sell.input", Version: 1}, Schema: json.RawMessage(`{"type":"object"}`)})
	return module
}

func TestPublishedProductFinderReferences(t *testing.T) {
	finder := func(view *View) *ViewComponent { return &view.Components[0] }
	tests := []struct {
		name     string
		mutate   func(*View)
		wantCode string
	}{
		{name: "valid product finder"},
		{name: "rank and categories are optional", mutate: func(v *View) {
			delete(finder(v).ResourceBindings, "rank")
			delete(finder(v).ResourceBindings, "categories")
			delete(finder(v).Bindings, "rank_key")
			delete(finder(v).Bindings, "category_label_field")
		}},
		{name: "category label is required with categories", mutate: func(v *View) {
			delete(finder(v).Bindings, "category_label_field")
		}, wantCode: "reference.component_binding_required"},
		{name: "items resource is required", mutate: func(v *View) { delete(finder(v).ResourceBindings, "items") }, wantCode: "reference.component_resource_required"},
		{name: "title is required", mutate: func(v *View) { delete(finder(v).Bindings, "title") }, wantCode: "reference.component_binding_required"},
		{name: "price is required", mutate: func(v *View) { delete(finder(v).Bindings, "price") }, wantCode: "reference.component_binding_required"},
		{name: "favourite outside allowed fields", mutate: func(v *View) { finder(v).Bindings["favourite"] = "is_favourite" }, wantCode: "reference.published_field_missing"},
		{name: "stock is not an item_search field", mutate: func(v *View) { finder(v).Bindings["stock"] = "quantity" }, wantCode: "reference.published_field_missing"},
		{name: "rank_key outside rank fields", mutate: func(v *View) { finder(v).Bindings["rank_key"] = "position" }, wantCode: "reference.published_field_missing"},
		{name: "term input is not a Studio binding", mutate: func(v *View) { finder(v).Bindings["term_input"] = "name" }, wantCode: "reference.component_binding_unsupported"},
		{name: "favourites resource is not a Studio role", mutate: func(v *View) {
			finder(v).ResourceBindings["favourites"] = "items"
		}, wantCode: "reference.component_resource_unsupported"},
		{name: "rank bound to a document query", mutate: func(v *View) {
			v.Resources[1] = PageResource{ID: "rank", Query: "document.list", Params: map[string]any{}}
		}, wantCode: "reference.component_resource_incompatible"},
		{name: "stock role with its bindings is valid", mutate: bindStockRole},
		{name: "stock_item is required with the stock role", mutate: func(v *View) {
			bindStockRole(v)
			delete(finder(v).Bindings, "stock_item")
		}, wantCode: "reference.component_binding_required"},
		{name: "stock_quantity is required with the stock role", mutate: func(v *View) {
			bindStockRole(v)
			delete(finder(v).Bindings, "stock_quantity")
		}, wantCode: "reference.component_binding_required"},
		{name: "stock_location is required with the stock role", mutate: func(v *View) {
			bindStockRole(v)
			delete(finder(v).Bindings, "stock_location")
		}, wantCode: "reference.component_binding_required"},
		{name: "stock_location outside item_stock fields", mutate: func(v *View) {
			bindStockRole(v)
			finder(v).Bindings["stock_location"] = "warehouse"
		}, wantCode: "reference.published_field_missing"},
		{name: "unknown published query", mutate: func(v *View) { v.Resources[0].Params["query_name"] = "nope" }, wantCode: "reference.published_query_missing"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			view, err := ParseViewYAML([]byte(productFinderRegisterViewYAML))
			if err != nil {
				t.Fatal(err)
			}
			if tc.mutate != nil {
				tc.mutate(view)
			}
			snapshot := &ConfigSnapshot{Views: []*View{view}, Modules: []contract.ModuleDescriptor{productFinderModule()}}
			findings := ValidateSnapshotReferences(snapshot, nil)
			var codes []string
			for _, finding := range findings {
				codes = append(codes, finding.Code)
			}
			if tc.wantCode == "" {
				if len(findings) != 0 {
					t.Fatalf("findings = %+v", findings)
				}
				return
			}
			if !strings.Contains(strings.Join(codes, ","), tc.wantCode) {
				t.Fatalf("codes = %v, want %s", codes, tc.wantCode)
			}
		})
	}
}

func TestPublishedProductFinderManifestCarriesVersionAndBindings(t *testing.T) {
	view, err := ParseViewYAML([]byte(productFinderRegisterViewYAML))
	if err != nil {
		t.Fatal(err)
	}
	manifest := PageManifestFromView(view)
	finder := manifest.Spec.Layout.Children[0]
	bindings, _ := finder.Props["bindings"].(map[string]string)
	if finder.Component != "product_finder" || finder.Version != 2 || finder.ResourceBindings["items"] != "items" ||
		finder.ResourceBindings["categories"] != "categories" || finder.ResourceBindings["rank"] != "rank" ||
		bindings["title"] != "item_name" || bindings["price"] != "price" || bindings["favourite"] != "favourite_position" ||
		bindings["category_label_field"] != "category_name" {
		t.Fatalf("product finder component = %+v", finder)
	}
}
