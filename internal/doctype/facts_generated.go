package doctype

// generatedFacts is the fact model a record type gets when its pack declares none: one row per record,
// keyed by creation, with every header number as a measure and every header Link, User, Select and Check as
// a dimension. A field whose column the fact table reserves is left out. A status Select is the status
// field. Child and single record types get no model, so it returns nil for them.
func generatedFacts(dt *DocType) *FactModel {
	if dt.IsChildTable || dt.IsSingle {
		return nil
	}
	f := &FactModel{Time: "creation", Generated: true}
	for _, fld := range dt.Fields {
		if reservedFactColumns[fld.Fieldname] {
			continue
		}
		switch fld.Fieldtype {
		case "Int", "Float", "Currency":
			f.Measures = append(f.Measures, FactMeasure{Field: fld.Fieldname})
		case "Link", "User", "Select", "Check":
			f.Dimensions = append(f.Dimensions, FactDimension{Field: fld.Fieldname})
		}
	}
	if sf := dt.GetField("status"); sf != nil && sf.Fieldtype == "Select" {
		f.StatusField = "status"
	}
	return f
}

// refreshGeneratedFacts gives dt its generated fact model, replacing a stale one, or drops it when the
// record type no longer qualifies. A model the pack declares is kept as written.
func (dt *DocType) refreshGeneratedFacts() {
	if dt.Facts != nil && !dt.Facts.Generated {
		return
	}
	dt.Facts = generatedFacts(dt)
}

// WithGeneratedFacts returns a copy of dt with the fact model the registry gives it on Register: the
// generated model when the pack declares none, the declared one otherwise. dt itself is not changed,
// so validating parsed record types never alters a registered one.
func (dt *DocType) WithGeneratedFacts() *DocType {
	cp := *dt
	cp.refreshGeneratedFacts()
	return &cp
}

// declaredFacts is the fact model the pack wrote: nil when the model is generated, so it is never saved.
func (dt *DocType) declaredFacts() *FactModel {
	if dt.Facts != nil && dt.Facts.Generated {
		return nil
	}
	return dt.Facts
}
