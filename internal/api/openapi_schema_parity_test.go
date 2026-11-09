package api

import (
	"testing"

	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/getkin/kin-openapi/openapi3"
)

func TestOpenAPIDoctypeSchemaMatchesSharedFieldSchema(t *testing.T) {
	reg := doctype.NewRegistry()
	parent := &doctype.DocType{
		Name: "Parent",
		Fields: []doctype.Field{
			{Fieldname: "title", Fieldtype: "Data"},
			{Fieldname: "count", Fieldtype: "Int"},
			{Fieldname: "price", Fieldtype: "Float"},
			{Fieldname: "active", Fieldtype: "Check"},
			{Fieldname: "status", Fieldtype: "Select", Options: "Draft\nSubmitted"},
		},
	}
	reg.Register(parent)

	openapiSchema := GenerateOpenAPISpec(reg, "acme").Components.Schemas["Parent"]
	sharedSchema := DocTypeToJSONSchema(parent, reg)

	wantProps, ok := sharedSchema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("shared schema properties missing: %#v", sharedSchema)
	}

	for fieldName, wantField := range wantProps {
		gotField, ok := openapiProperty(openapiSchema.Value, fieldName)
		if !ok {
			t.Fatalf("openapi schema missing field %q", fieldName)
		}
		if !openapiFieldMatchesJSONSchema(gotField, wantField.(map[string]any)) {
			t.Fatalf("field %q schema mismatch\nopenapi: %#v\nshared: %#v", fieldName, gotField, wantField)
		}
	}
}

func openapiProperty(schema *openapi3.Schema, fieldName string) (*openapi3.Schema, bool) {
	if schema == nil {
		return nil, false
	}
	propRef, ok := schema.Properties[fieldName]
	if !ok || propRef == nil || propRef.Value == nil {
		return nil, false
	}
	return propRef.Value, true
}

func openapiFieldMatchesJSONSchema(got *openapi3.Schema, want map[string]any) bool {
	if got == nil {
		return false
	}
	if got.Type == nil || !got.Type.Is(wantType(want["type"])) {
		return false
	}
	if enumVals, ok := want["enum"].([]string); ok {
		if len(got.Enum) != len(enumVals) {
			return false
		}
		for i, v := range enumVals {
			if got.Enum[i] != v {
				return false
			}
		}
	}
	if wantType(want["type"]) == "array" {
		if got.Items == nil || got.Items.Value == nil {
			return false
		}
		if got.Items.Value.Type == nil || !got.Items.Value.Type.Is("object") {
			return false
		}
	}
	return true
}

func wantType(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
