package doctype

import (
	"fmt"
	"strings"
)

const (
	DefaultPublicMaxLimit  = 50
	AbsolutePublicMaxLimit = 100
	DefaultPublicCacheAge  = 60
	AbsolutePublicCacheAge = 300
)

var publicSystemFields = map[string]bool{
	"name":       true,
	"creation":   true,
	"modified":   true,
	"doc_status": true,
}

func (d *DocType) NormalizePublicAccess() {
	if d.PublicAccess == nil {
		return
	}
	pa := d.PublicAccess
	if pa.MaxLimit <= 0 {
		pa.MaxLimit = DefaultPublicMaxLimit
	}
	if pa.MaxLimit > AbsolutePublicMaxLimit {
		pa.MaxLimit = AbsolutePublicMaxLimit
	}
	if pa.CacheMaxAge <= 0 {
		pa.CacheMaxAge = DefaultPublicCacheAge
	}
	if pa.CacheMaxAge > AbsolutePublicCacheAge {
		pa.CacheMaxAge = AbsolutePublicCacheAge
	}
	if pa.SortField == "" {
		pa.SortField = d.SortField
	}
	if pa.SortField == "" {
		pa.SortField = "modified"
	}
	if pa.SortOrder == "" {
		pa.SortOrder = d.SortOrder
	}
	if pa.SortOrder == "" {
		pa.SortOrder = "DESC"
	}
	pa.SortOrder = strings.ToUpper(pa.SortOrder)
}

func (d *DocType) ValidatePublicAccess() error {
	if d.PublicAccess == nil || !d.PublicAccess.Enabled {
		return nil
	}
	d.NormalizePublicAccess()
	pa := d.PublicAccess
	if len(pa.Fields) == 0 {
		return fmt.Errorf("public_access.fields must list at least one field")
	}
	for _, field := range pa.Fields {
		if err := d.validatePublicField(field); err != nil {
			return err
		}
	}
	for _, filter := range pa.Filters {
		if filter.Field == "" {
			return fmt.Errorf("public_access filter is missing field")
		}
		if err := d.validatePublicScalarField(filter.Field); err != nil {
			return err
		}
		switch strings.ToLower(strings.TrimSpace(filter.Op)) {
		case "equals", "not_equals", "in", "is_set", "is_not_set":
		default:
			return fmt.Errorf("unsupported public_access filter operator %q", filter.Op)
		}
	}
	if err := d.validatePublicScalarField(pa.SortField); err != nil {
		return fmt.Errorf("invalid public_access sort_field: %w", err)
	}
	switch pa.SortOrder {
	case "ASC", "DESC":
	default:
		return fmt.Errorf("public_access sort_order must be ASC or DESC")
	}
	return nil
}

func (d *DocType) validatePublicField(field string) error {
	if publicSystemFields[field] {
		return nil
	}
	f := d.GetField(field)
	if f == nil {
		return fmt.Errorf("public_access field %q does not exist on %s", field, d.Name)
	}
	if f.Fieldtype == "Password" {
		return fmt.Errorf("public_access field %q cannot expose Password fields", field)
	}
	if f.Hidden {
		return fmt.Errorf("public_access field %q cannot expose hidden fields", field)
	}
	if f.Fieldtype == "Table" {
		return fmt.Errorf("public_access field %q cannot expose Table fields in v1", field)
	}
	return nil
}

func (d *DocType) validatePublicScalarField(field string) error {
	if publicSystemFields[field] || field == "owner" || field == "modified_by" {
		return nil
	}
	f := d.GetField(field)
	if f == nil {
		return fmt.Errorf("field %q does not exist on %s", field, d.Name)
	}
	if f.Fieldtype == "Table" {
		return fmt.Errorf("field %q cannot be used for public filters or sorting", field)
	}
	return nil
}

func (d *DocType) PublicFieldSet() map[string]bool {
	set := make(map[string]bool)
	if d.PublicAccess == nil {
		return set
	}
	for _, field := range d.PublicAccess.Fields {
		set[field] = true
	}
	return set
}
