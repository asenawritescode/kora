package kernel

import (
	"encoding/json"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/doctype"
)

func BenchmarkPayloadHash(b *testing.B) {
	payload := json.RawMessage(`{"doctype":"Sale","data":{"customer":"CUS-0001","items":[{"product":"PROD-0001","quantity":2,"price":250.5}]}}`)
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for range b.N {
		_ = payloadHash(payload)
	}
}

func BenchmarkCommandPayloadEncodeDecode(b *testing.B) {
	input := map[string]any{
		"customer": "CUS-0001",
		"items":    []any{map[string]any{"product": "PROD-0001", "quantity": float64(2), "price": 250.5}},
	}
	payload, err := json.Marshal(map[string]any{"data": input})
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for range b.N {
		decoded, decodeErr := decodeInput(payload)
		if decodeErr != nil {
			b.Fatal(decodeErr)
		}
		if _, encodeErr := json.Marshal(map[string]any{"data": decoded}); encodeErr != nil {
			b.Fatal(encodeErr)
		}
	}
}

func BenchmarkOperationAuthorization(b *testing.B) {
	dt := &doctype.DocType{Name: "Sale"}
	reg := doctype.NewRegistry()
	reg.LoadFull(
		[]*doctype.DocType{dt},
		[]*doctype.Role{{Name: doctype.AdminRole}},
		[]*doctype.Permission{{Doctype: dt.Name, Role: doctype.AdminRole, Create: true}},
	)
	op := Operation{
		Context: OperationContext{Roles: []string{doctype.AdminRole}},
		Command: CommandRecordCreate,
		Payload: json.RawMessage(`{"doctype":"Sale","data":{"customer":"CUS-0001"}}`),
	}
	definition, ok := LookupCommand(CommandRecordCreate)
	if !ok {
		b.Fatal("record.create command definition missing")
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if err := authorizeOp(reg, op, definition, nil); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkOperationValidation(b *testing.B) {
	dt := &doctype.DocType{Name: "Sale", Fields: []doctype.Field{
		{Fieldname: "customer", Fieldtype: "Data", Reqd: true},
		{Fieldname: "total", Fieldtype: "Currency", Reqd: true},
		{Fieldname: "note", Fieldtype: "Small Text"},
	}}
	reg := doctype.NewRegistry()
	reg.Register(dt)
	doc := doctype.NewDocument(dt.Name)
	doc.Set("customer", "CUS-0001")
	doc.Set("total", 501.0)
	doc.Set("note", "cash sale")
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if validation := doctype.ValidateDocument(dt, doc, reg, nil); validation.HasErrors() {
			b.Fatal(contract.CodeValidationFailed)
		}
	}
}
