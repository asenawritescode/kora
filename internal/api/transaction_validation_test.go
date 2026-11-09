package api

import (
	"net/http/httptest"
	"testing"

	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/gin-gonic/gin"
)

func TestApplyTransactionTotalsUsesConfiguredLineValues(t *testing.T) {
	parent := &doctype.DocType{Name: "Sale", Fields: []doctype.Field{{Fieldname: "total_amount", Fieldtype: "Currency"}}}
	doc := doctype.NewDocument("")
	line := doctype.NewDocument("")
	line.Set("quantity", 2)
	line.Set("unit_price", 12.5)
	line.Set("line_amount", 99999.0) // browser-supplied amount must not be trusted.
	if errs := applyTransactionTotals(parent, doc, []*doctype.Document{line}, map[string]any{
		"totals": map[string]any{
			"lines":       map[string]any{"quantity_field": "quantity", "unit_price_field": "unit_price", "line_total_field": "line_amount"},
			"total_field": "total_amount",
		},
	}); errs.HasErrors() {
		t.Fatalf("unexpected total validation errors: %#v", errs)
	}
	if got := line.GetFloat("line_amount"); got != 25 {
		t.Fatalf("line amount = %v, want 25", got)
	}
	if got := doc.GetFloat("total_amount"); got != 25 {
		t.Fatalf("sale total = %v, want 25", got)
	}
}

func TestApplyTransactionTotalsRejectsOverflow(t *testing.T) {
	parent := &doctype.DocType{Name: "Sale", Fields: []doctype.Field{{Fieldname: "total_amount", Fieldtype: "Currency"}}}
	doc := doctype.NewDocument("Sale")
	line := doctype.NewDocument("Sale Item")
	line.Set("quantity", 2)
	line.Set("unit_price", 1e308)
	errs := applyTransactionTotals(parent, doc, []*doctype.Document{line}, map[string]any{
		"totals": map[string]any{
			"lines":       map[string]any{"quantity_field": "quantity", "unit_price_field": "unit_price", "line_total_field": "line_amount"},
			"total_field": "total_amount",
		},
	})
	if !errs.HasErrors() {
		t.Fatal("overflowed line amount was accepted")
	}
}

func TestValidateTransactionChildrenRejectsInvalidAmountsAndQuantities(t *testing.T) {
	childType := &doctype.DocType{Name: "Sale Item", IsChildTable: true, Fields: []doctype.Field{
		{Fieldname: "quantity", Fieldtype: "Int", Label: "Quantity", Reqd: true, Constraints: []doctype.Constraint{{Type: "min", Value: 1, Message: "Quantity must be at least 1."}}},
		{Fieldname: "unit_price", Fieldtype: "Currency", Label: "Unit Price", Reqd: true, Constraints: []doctype.Constraint{{Type: "min", Value: 0, Message: "Unit price cannot be negative."}}},
	}}
	line := doctype.NewDocument("")
	line.Set("quantity", 0)
	line.Set("unit_price", -2)
	errList := validateTransactionChildren(childType, []*doctype.Document{line}, nil)
	if len(errList) < 2 {
		t.Fatalf("expected quantity and price errors, got %#v", errList)
	}
	line.Set("quantity", 1.5)
	line.Set("unit_price", 10)
	errList = validateTransactionChildren(childType, []*doctype.Document{line}, nil)
	if len(errList) == 0 {
		t.Fatal("fractional value in Int field was accepted")
	}
	line.Set("quantity", 1)
	line.Set("unit_price", "NaN")
	errList = validateTransactionChildren(childType, []*doctype.Document{line}, nil)
	if len(errList) == 0 {
		t.Fatal("non-finite unit price was accepted")
	}
}

func TestTransactionIdempotencyKeyUsesUniqueRequestHeader(t *testing.T) {
	action := &doctype.ViewAction{ID: "complete_sale", Config: map[string]any{"idempotency_prefix": "pos-sale"}}
	doc := doctype.NewDocument("")
	first, err := transactionIdempotencyKey(transactionContextWithHeader("attempt-1"), action, doc)
	if err != nil {
		t.Fatal(err)
	}
	second, err := transactionIdempotencyKey(transactionContextWithHeader("attempt-2"), action, doc)
	if err != nil {
		t.Fatal(err)
	}
	if first != "pos-sale:attempt-1" || second != "pos-sale:attempt-2" {
		t.Fatalf("unexpected transaction keys %q and %q", first, second)
	}
}

func transactionContextWithHeader(key string) *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/", nil)
	c.Request.Header.Set("Idempotency-Key", key)
	return c
}
