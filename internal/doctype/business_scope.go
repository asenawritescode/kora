package doctype

import (
	"fmt"
	"strings"

	"github.com/asenawritescode/kora/contract"
)

// BindBusinessScope applies the verified entity to a new entity-owned record
// and prevents updates from moving records between entities.
func (d *DocType) BindBusinessScope(doc *Document, scope contract.BusinessScope, creating bool, old *Document) error {
	if d == nil || d.BusinessScopePolicy != contract.ScopeEntityOwned {
		return nil
	}
	if doc == nil || d.BusinessScopeField == "" || d.GetField(d.BusinessScopeField) == nil {
		return fmt.Errorf("entity-owned resource has no valid entity scope field")
	}
	if err := contract.ValidateResourceBusinessScope(scope, d.BusinessScopePolicy); err != nil {
		return err
	}
	field := d.BusinessScopeField
	want := scope.LegalEntityID
	value := strings.TrimSpace(fmt.Sprint(doc.Get(field)))
	if value == "<nil>" {
		value = ""
	}
	if creating {
		if value != "" && value != want {
			return fmt.Errorf("record entity does not match verified business scope")
		}
		doc.Set(field, want)
		return nil
	}
	if old == nil || strings.TrimSpace(fmt.Sprint(old.Get(field))) != want || value != want {
		return fmt.Errorf("record entity cannot be changed or accessed outside verified business scope")
	}
	return nil
}
