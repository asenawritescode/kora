//go:build integration

package api

import (
	"net/http"
	"testing"

	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/schema"
)

func TestConfiguredRelatedQuantityUpdateReplaysOnceAndRejectsOversell(t *testing.T) {
	h, database, registry, site := newResourceIntegrationHandler(t)
	registerPOSIntegrationTypes(t, database, site, registry)
	registry.Get("Payment").Fields = append(registry.Get("Payment").Fields, doctype.Field{Fieldname: "register", Fieldtype: "Data"})
	registry.Register(&doctype.DocType{Name: "Inventory Bin", Fields: []doctype.Field{
		{Fieldname: "label", Fieldtype: "Data", Reqd: true},
		{Fieldname: "units", Fieldtype: "Int", Reqd: true, ReadOnly: true},
	}})
	if err := schema.MigrateSiteFromRegistry(database, site, registry, kdb.Resolve("mysql")); err != nil {
		t.Fatal(err)
	}
	created := serveResourceMutationBody(t, h, database, registry, site, httpMethodPost, "/api/resource/Inventory%20Bin", `{"label":"Test shelf","units":10}`, "bin-key", []string{doctype.AdminRole})
	if created.Code != http.StatusCreated {
		t.Fatalf("create bin: HTTP %d: %s", created.Code, created.Body.String())
	}
	binName := requireRESTDocument(t, created.Body.Bytes(), "Inventory Bin")["name"].(string)
	registry.Views.Register(&doctype.View{Name: "Configured checkout", Route: "/configured-checkout", Type: "register", SourceDocType: "Sale", Components: []doctype.ViewComponent{{ID: "payment", Actions: []doctype.ViewAction{{
		ID: "complete", Trigger: "on_click", Type: "create_transaction", Config: map[string]any{
			"target_doctype": "Sale", "reference_field": "reference", "idempotency_prefix": "configured-checkout",
			"allowed_context_values": map[string]any{"register": []any{"REG-TEST"}, "payment_method": []any{"cash"}},
			"related_records": []any{map[string]any{"key": "payment", "doctype": "Payment", "fields": map[string]any{
				"reference": "PAY-$records.sale.name", "sale": "$records.sale.name", "amount": "$context.total", "method": "$context.payment_method", "register": "$context.register", "assignee": "$actor",
			}}},
			"child_table": "Sale Item", "line_source": "cart",
			"line_fields":              map[string]any{"product": "product", "unit_price": "rate", "quantity": "quantity"},
			"totals":                   map[string]any{"subtotal_field": "subtotal", "total_field": "total", "lines": map[string]any{"quantity_field": "quantity", "unit_price_field": "unit_price", "line_total_field": "line_total"}},
			"related_quantity_updates": map[string]any{"target_doctype": "Inventory Bin", "target_quantity_field": "units", "source_record_field": "product", "source_quantity_field": "quantity", "whole_units": true},
		},
	}}}}})
	checkout := map[string]any{"reference": "TEST-CONFIG-QUANTITY-1", "cart": []any{map[string]any{"product": binName, "rate": 5, "quantity": 3}}, "payment_method": "cash", "register": "REG-TEST", "total": 15}
	first := serveManifestAction(t, h, site, database, registry, "complete", checkout)
	if first.Code != http.StatusOK {
		t.Fatalf("checkout: HTTP %d: %s", first.Code, first.Body.String())
	}
	name := requireActionDocument(t, first.Body.Bytes())["name"].(string)
	replay := serveManifestAction(t, h, site, database, registry, "complete", checkout)
	if replay.Code != http.StatusOK || replay.Header().Get("X-Kora-Replay") != "true" || requireActionDocument(t, replay.Body.Bytes())["name"] != name {
		t.Fatalf("checkout replay: HTTP %d: %s", replay.Code, replay.Body.String())
	}
	var units, saleCount, paymentCount int
	if err := database.QueryRow("SELECT units FROM `tabInventory Bin` WHERE name = ?", binName).Scan(&units); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow("SELECT COUNT(*) FROM `tabSale`").Scan(&saleCount); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow("SELECT COUNT(*) FROM `tabPayment`").Scan(&paymentCount); err != nil {
		t.Fatal(err)
	}
	if units != 7 || saleCount != 1 || paymentCount != 1 {
		t.Fatalf("accepted/replayed effects units=%d sale=%d payment=%d", units, saleCount, paymentCount)
	}
	checkout["reference"] = "TEST-CONFIG-QUANTITY-2"
	checkout["cart"] = []any{map[string]any{"product": binName, "rate": 5, "quantity": 8}}
	rejected := serveManifestAction(t, h, site, database, registry, "complete", checkout)
	if rejected.Code != http.StatusBadRequest {
		t.Fatalf("oversell status = %d: %s", rejected.Code, rejected.Body.String())
	}
	if err := database.QueryRow("SELECT units FROM `tabInventory Bin` WHERE name = ?", binName).Scan(&units); err != nil {
		t.Fatal(err)
	}
	if units != 7 {
		t.Fatalf("oversell changed quantity to %d", units)
	}
	checkout["reference"] = "TEST-CONFIG-QUANTITY-3"
	checkout["cart"] = []any{map[string]any{"product": binName, "rate": 5, "quantity": 1}}
	checkout["register"] = "REG-OTHER"
	wrongRegister := serveManifestAction(t, h, site, database, registry, "complete", checkout)
	if wrongRegister.Code != http.StatusBadRequest {
		t.Fatalf("ineligible register status = %d: %s", wrongRegister.Code, wrongRegister.Body.String())
	}
}
