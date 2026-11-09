package api

import (
	"encoding/json"
	"testing"

	"github.com/asenawritescode/kora/internal/doctype"
)

func TestResourceProjectionKeepsExactDecimals(t *testing.T) {
	dt := &doctype.DocType{Name: "Invoice", Fields: []doctype.Field{
		{Fieldname: "total", Fieldtype: "Currency"},
		{Fieldname: "weight", Fieldtype: "Float"},
		{Fieldname: "rate", Fieldtype: "Percent"},
	}}
	doc := doctype.NewDocument(dt.Name)
	doc.Name = "INV-1"
	doc.Set("total", "1000000000000.125000000")
	doc.Set("weight", json.Number("0.125"))
	doc.Set("rate", 16.0)

	out, err := json.Marshal(docToMap(doc, dt, nil, []string{"total", "weight", "rate"}))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	for field, want := range map[string]string{"total": "1000000000000.125", "weight": "0.125", "rate": "16"} {
		if string(got[field]) != want {
			t.Fatalf("%s = %s, want JSON number %s", field, got[field], want)
		}
	}
}
