package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/orm"
)

// bundle is a record.mutate_bundle command after its payload was validated
// and its records named.
type bundle struct {
	payload RecordMutationBundlePayload
	// names maps each bundle item key to the name of its record.
	names map[string]string
}

// openBundle validates the payload, loads the hooks of every doctype it
// touches, claims the idempotency receipt and names every record.
func (m *mutation) openBundle(ctx context.Context) (*bundle, *contract.Error) {
	p, cerr := m.bundlePayload()
	if cerr != nil {
		return nil, cerr
	}
	for _, item := range p.Records {
		recordDT := m.run.Reg.Get(item.Doctype)
		if err := m.run.Mgr.LoadLifecycleHooksTx(ctx, m.run.Tx, recordDT); err != nil {
			return nil, contract.NewError(contract.CodeDependencyUnavailable, err.Error())
		}
	}
	// Claim the idempotency key before allocating names or performing other
	// transactional writes. Concurrent retries must contend on the unique
	// receipt first; otherwise SQLite-compatible dialects can report a name
	// allocator lock error before the duplicate is recognized as a replay.
	if claimErr := m.claim(); claimErr != nil {
		return nil, claimErr
	}
	names, cerr := m.bundleNames(p)
	if cerr != nil {
		return nil, cerr
	}
	return &bundle{payload: p, names: names}, nil
}

// bundlePayload decodes the payload and checks its shape: a primary record of
// the command's doctype first, then uniquely keyed items of known doctypes.
func (m *mutation) bundlePayload() (RecordMutationBundlePayload, *contract.Error) {
	var p RecordMutationBundlePayload
	if err := json.Unmarshal(m.op.Payload, &p); err != nil || len(p.Records) == 0 || len(p.Records) > MaxMutationBundleRecords || p.Doctype != m.dt.Name || p.Records[0].Doctype != m.dt.Name {
		return p, contract.NewError(contract.CodeValidationFailed, "invalid record.mutate_bundle payload")
	}
	keySet := make(map[string]struct{}, len(p.Records))
	for _, item := range p.Records {
		if item.Key == "" || item.Doctype == "" {
			return p, contract.NewError(contract.CodeValidationFailed, "bundle record key and doctype are required")
		}
		if _, exists := keySet[item.Key]; exists {
			return p, contract.NewError(contract.CodeValidationFailed, "bundle record keys must be unique")
		}
		if m.run.Reg.Get(item.Doctype) == nil {
			return p, contract.NewError(contract.CodeNotFound, fmt.Sprintf("doctype %q not found", item.Doctype))
		}
		keySet[item.Key] = struct{}{}
	}
	return p, nil
}

// bundleNames allocates the name of every created record and resolves the
// name of every existing one, in item order so later items can refer to
// earlier ones.
func (m *mutation) bundleNames(p RecordMutationBundlePayload) (map[string]string, *contract.Error) {
	names := make(map[string]string, len(p.Records))
	for _, item := range p.Records {
		operation := item.Operation
		if operation == "" {
			operation = "create"
		}
		if operation != "create" {
			name, resolveErr := resolveBundleName(item.Name, names)
			if resolveErr != nil {
				return nil, contract.NewError(contract.CodeValidationFailed, resolveErr.Error())
			}
			names[item.Key] = name
			continue
		}
		recordDT := m.run.Reg.Get(item.Doctype)
		name, _, allocationErr := orm.AllocateDocumentName(m.run.Tx, m.k.dialect(), recordDT, orm.DocumentValues(item.Data), time.Now().UTC())
		if allocationErr != nil {
			var validation *doctype.ValidationError
			if errors.As(allocationErr, &validation) {
				return nil, contract.NewError(contract.CodeValidationFailed, validation.Message)
			}
			return nil, contract.NewError(contract.CodeDependencyUnavailable, "allocate bundle record name failed")
		}
		names[item.Key] = name
	}
	return names, nil
}
