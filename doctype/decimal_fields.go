package doctype

import "fmt"

// MaxDecimalPrecision matches the scale of the DECIMAL(21,9) storage columns.
const MaxDecimalPrecision = 9

// validateDecimalFields checks precision and currency metadata. precision is
// the number of decimal places a Float, Currency or Percent value is rounded to
// on save. A Currency field's options may name a field on the same record that
// holds its currency code; empty options mean the tenant default currency.
func (d *DocType) validateDecimalFields() error {
	for _, f := range d.Fields {
		decimal := f.Fieldtype == "Float" || f.Fieldtype == "Currency" || f.Fieldtype == "Percent"
		if f.Precision != nil {
			if !decimal {
				return fmt.Errorf("doctype %s, field %s: precision applies only to Float, Currency and Percent fields", d.Name, f.Fieldname)
			}
			if *f.Precision < 0 || *f.Precision > MaxDecimalPrecision {
				return fmt.Errorf("doctype %s, field %s: precision must be between 0 and %d", d.Name, f.Fieldname, MaxDecimalPrecision)
			}
		}
		if f.Fieldtype != "Currency" || f.Options == "" {
			continue
		}
		currencyField := d.GetField(f.Options)
		if currencyField == nil {
			return fmt.Errorf("doctype %s, field %s: options must name the field holding the currency code, %q does not exist", d.Name, f.Fieldname, f.Options)
		}
		switch currencyField.Fieldtype {
		case "Link", "Data", "Select":
		default:
			return fmt.Errorf("doctype %s, field %s: currency field %q must be a Link, Data or Select field", d.Name, f.Fieldname, f.Options)
		}
	}
	return nil
}
