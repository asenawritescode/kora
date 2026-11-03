package doctype

import (
	"strings"
	"time"
)

func ApplyDefaultValues(dt *DocType, doc *Document) {
	if dt == nil || doc == nil {
		return
	}
	for _, field := range dt.DataFields() {
		if field.Default == "" {
			continue
		}
		if _, exists := doc.Fields[field.Fieldname]; exists {
			continue
		}
		value := any(field.Default)
		switch strings.ToLower(strings.TrimSpace(field.Default)) {
		case "now":
			switch field.Fieldtype {
			case "Datetime":
				value = time.Now().UTC()
			case "Time":
				value = time.Now().UTC().Format("15:04:05.000000")
			}
		case "today":
			switch field.Fieldtype {
			case "Date":
				value = time.Now().UTC().Format("2006-01-02")
			case "Datetime":
				value = time.Now().UTC()
			}
		}
		doc.Set(field.Fieldname, value)
	}
}
