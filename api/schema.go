// Package api provides shared schema mapping from Kora field types to JSON Schema.
// Used by OpenAPI, MCP, and Chat to generate consistent tool/schema definitions.
package api

import "github.com/asenawritescode/kora/doctype"

// FieldToJSONSchema maps a Kora field definition to a JSON Schema property.
func FieldToJSONSchema(f *doctype.Field) map[string]any {
	schema := map[string]any{}

	switch f.Fieldtype {
	case "Data", "Password":
		schema["type"] = "string"
		schema["maxLength"] = 140
	case "Text":
		schema["type"] = "string"
	case "Text Editor":
		schema["type"] = "string"
		schema["format"] = "text-editor"
	case "Int":
		schema["type"] = "integer"
		schema["format"] = "int64"
	case "Float", "Currency", "Percent":
		schema["type"] = "number"
		schema["format"] = "double"
	case "Check":
		schema["type"] = "boolean"
	case "Date":
		schema["type"] = "string"
		schema["format"] = "date"
	case "Time":
		schema["type"] = "string"
		schema["format"] = "time"
	case "Datetime":
		schema["type"] = "string"
		schema["format"] = "date-time"
	case "Select":
		schema["type"] = "string"
		if f.Options != "" {
			// Options are newline-separated values.
			var enumVals []string
			for _, v := range splitLines(f.Options) {
				if v != "" {
					enumVals = append(enumVals, v)
				}
			}
			if len(enumVals) > 0 {
				schema["enum"] = enumVals
			}
		}
	case "Link", "Dynamic Link":
		schema["type"] = "string"
		if f.Options != "" {
			schema["description"] = "Link to " + f.Options
		}
	case "Table":
		schema["type"] = "array"
		schema["items"] = map[string]string{"$ref": "#/components/schemas/" + f.Options}
	case "Attach", "Attach Image":
		schema["type"] = "string"
		schema["format"] = "uri"
	case "JSON":
		schema["type"] = "object"
	default:
		schema["type"] = "string"
	}

	if f.Description != "" {
		schema["description"] = f.Description
	}
	return schema
}

// DocTypeToJSONSchema converts a DocType into a JSON Schema object.
func DocTypeToJSONSchema(dt *doctype.DocType, registry *doctype.Registry) map[string]any {
	props := make(map[string]any)
	required := []string{}

	for _, f := range dt.DataFields() {
		if f.Fieldtype == "Table" {
			// Child table — recursively generate schema for the child doctype.
			if childDT := registry.Get(f.Options); childDT != nil {
				props[f.Fieldname] = map[string]any{
					"type":  "array",
					"items": DocTypeToJSONSchema(childDT, registry),
				}
			}
			continue
		}
		props[f.Fieldname] = FieldToJSONSchema(&f)
		if f.Reqd {
			required = append(required, f.Fieldname)
		}
	}

	schema := map[string]any{
		"type":       "object",
		"properties": props,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}
