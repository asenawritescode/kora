package kernel

import (
	"encoding/json"
	"fmt"

	"github.com/asenawritescode/kora/contract"
)

func newHistoricalSnapshotValidator(scope contract.BusinessScope, schemas CommandSchemaRegistry) contract.HistoricalSnapshotValidator {
	if scope.Validate() != nil || schemas == nil {
		return nil
	}
	return func(expected contract.SchemaRef, snapshot contract.HistoricalSnapshot) error {
		if snapshot.BusinessScope != scope {
			return fmt.Errorf("historical snapshot scope does not match verified operation scope")
		}
		if snapshot.Schema != expected {
			return fmt.Errorf("historical snapshot schema does not match field declaration")
		}
		if !schemas.Registered(expected) {
			return fmt.Errorf("historical snapshot schema is not registered")
		}
		return contract.ValidateHistoricalSnapshot(snapshot, func(schema contract.SchemaRef, data json.RawMessage) error {
			if schema != expected {
				return fmt.Errorf("historical snapshot schema does not match field declaration")
			}
			return schemas.Validate(schema, data)
		})
	}
}
