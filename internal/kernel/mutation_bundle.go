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
	"github.com/asenawritescode/kora/internal/workflow"
)

// bundle is a record.mutate_bundle command after its payload was validated
// and its records named.
type bundle struct {
	payload RecordMutationBundlePayload
	// names maps each bundle item key to the name of its record.
	names map[string]string
	// docs, types and pending hold the prepared record of every item, its
	// doctype, and the same records by doctype and name for link lookups.
	docs    []*doctype.Document
	types   []*doctype.DocType
	pending map[string]*doctype.Document
}

// start sizes the prepared-record tables for the payload.
func (b *bundle) start() {
	n := len(b.payload.Records)
	b.docs = make([]*doctype.Document, n)
	b.types = make([]*doctype.DocType, n)
	b.pending = make(map[string]*doctype.Document, n)
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

// authorizeItem checks the item's operation and the caller's permission for
// it, and returns the operation with its default applied.
func (m *mutation) authorizeItem(item RecordMutationBundleItem, recordDT *doctype.DocType) (string, *contract.Error) {
	operation := item.Operation
	if operation == "" {
		operation = "create"
	}
	if operation != "create" && operation != "update" && operation != "decrement" && operation != "adjust" {
		return "", contract.NewError(contract.CodeValidationFailed, "bundle operation must be create, update, decrement, or adjust")
	}
	permission := "create"
	if operation != "create" {
		permission = "write"
	}
	if !m.op.Context.isEffectItem(item.Key) {
		if _, permissionErr := decideOperationResource(m.op, m.run.Reg, recordDT, permission); permissionErr != nil {
			return "", permissionErr
		}
	}
	if item.TransitionAction != "" {
		if operation != "update" {
			return "", contract.NewError(contract.CodeValidationFailed, "transition actions require an update bundle item")
		}
		if _, permissionErr := decideOperationResource(m.op, m.run.Reg, recordDT, "submit"); permissionErr != nil {
			return "", permissionErr
		}
	}
	return operation, nil
}

// loadItem reads the record an update, decrement or adjust item changes and
// checks the revision and version the caller expects of it.
func (m *mutation) loadItem(item RecordMutationBundleItem, i int, recordDT *doctype.DocType, name string) (*doctype.Document, *contract.Error) {
	reg, txMgr := m.run.Reg, m.run.Mgr
	oldDoc, err := txMgr.GetDocWithScope(recordDT, name, scopeForOperation(reg, m.op, recordDT.Name, "write")) //nolint:contextcheck // the context travels in txMgr.Context
	if err != nil {
		if errors.Is(err, orm.ErrNotFound) {
			return nil, contract.NewError(contract.CodeNotFound, "document not found")
		}
		return nil, contract.NewError(contract.CodeDependencyUnavailable, "load bundle document failed")
	}
	if item.TransitionAction != "" {
		if _, err = txMgr.GetDocWithScope(recordDT, name, scopeForOperation(reg, m.op, recordDT.Name, "submit")); err != nil { //nolint:contextcheck // the context travels in txMgr.Context
			if errors.Is(err, orm.ErrNotFound) {
				return nil, contract.NewError(contract.CodeNotFound, "document not found")
			}
			return nil, contract.NewError(contract.CodeDependencyUnavailable, "load workflow document failed")
		}
	}
	if item.ExpectedRevision > 0 && oldDoc.Revision != item.ExpectedRevision {
		m.auditFailure(ErrStaleVersion.Type)
		return nil, ErrStaleVersion
	}
	if i == 0 {
		if cerr := m.checkItemVersion(recordDT, name); cerr != nil {
			return nil, cerr
		}
	}
	return oldDoc, nil
}

// checkItemVersion compares the caller's expected version with the stored
// row of the bundle's primary record.
func (m *mutation) checkItemVersion(recordDT *doctype.DocType, name string) *contract.Error {
	expectedVersion := expectedVersionFrom(m.op)
	if expectedVersion == "" {
		return nil
	}
	currentVersion, err := readRowVersion(m.run.Tx, m.k.dialect(), recordDT.RawTableName(), name)
	if err != nil {
		return contract.NewError(contract.CodeDependencyUnavailable, "read bundle record version failed")
	}
	if currentVersion != expectedVersion {
		m.auditFailure(ErrStaleVersion.Type)
		return ErrStaleVersion
	}
	return nil
}

// buildItem prepares the record of bundle item i: authorizes it, loads what
// it changes, applies its data and any workflow action, and registers it for
// the validation and persistence passes.
func (m *mutation) buildItem(ctx context.Context, b *bundle, i int) *contract.Error {
	item := b.payload.Records[i]
	recordDT := m.run.Reg.Get(item.Doctype)
	operation, cerr := m.authorizeItem(item, recordDT)
	if cerr != nil {
		return cerr
	}
	data, err := resolveCreateBundleData(item.Data, b.names)
	if err != nil {
		return contract.NewError(contract.CodeValidationFailed, fmt.Sprintf("bundle record %q: %v", item.Key, err))
	}
	if operation == "create" {
		if rejectErr := m.k.rejectWorkflowStateOnCreate(ctx, m.run.Tx, m.op, recordDT, data); rejectErr != nil {
			return contract.NewError(contract.CodeValidationFailed, fmt.Sprintf("bundle record %q: %v", item.Key, rejectErr))
		}
	}
	doc := doctype.NewDocument(recordDT.Name)
	doc.Name = b.names[item.Key]
	var oldDoc *doctype.Document
	if operation != "create" {
		if oldDoc, cerr = m.loadExisting(ctx, b, i, recordDT, data); cerr != nil {
			return cerr
		}
		doc = cloneDoc(oldDoc)
	}
	if data, cerr = m.itemData(recordDT, oldDoc, operation, data); cerr != nil {
		return cerr
	}
	if cerr := m.fillItem(recordDT, operation, doc, oldDoc, data); cerr != nil {
		return cerr
	}
	if item.TransitionAction != "" {
		if cerr := m.applyItemAction(ctx, recordDT, doc, oldDoc, item.TransitionAction); cerr != nil {
			return cerr
		}
	}
	if operation == "create" {
		setDefaultValues(recordDT, doc)
	} else {
		doc.IsNew = false
	}
	b.docs[i], b.types[i] = doc, recordDT
	b.pending[recordDT.Name+"\x00"+doc.Name] = doc
	m.bundleSavedDocs = append(m.bundleSavedDocs, savedBundleDocument{dt: recordDT, doc: doc, oldDoc: oldDoc, workflowTransition: item.TransitionAction != ""})
	return nil
}

// itemData turns the data of a decrement or adjust item into the absolute
// values to write; other operations keep their data as it is.
func (m *mutation) itemData(recordDT *doctype.DocType, oldDoc *doctype.Document, operation string, data json.RawMessage) (json.RawMessage, *contract.Error) {
	if operation != "decrement" && operation != "adjust" {
		return data, nil
	}
	adjusted, err := adjustQuantity(recordDT, oldDoc, operation, data)
	if err != nil {
		return nil, contract.NewError(contract.CodeValidationFailed, err.Error())
	}
	return adjusted, nil
}

// applyItemAction moves the record to the state a workflow action leads to.
func (m *mutation) applyItemAction(ctx context.Context, recordDT *doctype.DocType, doc, oldDoc *doctype.Document, action string) *contract.Error {
	statusPolicy, unified, policyErr := m.k.workflowStatusPolicy(ctx, m.run.Tx, m.op, recordDT.Name)
	if policyErr != nil {
		return contract.NewError(contract.CodeDependencyUnavailable, "load workflow status policy failed")
	}
	if !unified || statusPolicy == nil {
		return contract.NewError(contract.CodeValidationFailed, "no published status transition policy for table")
	}
	stateField := statusPolicy.StateField
	if recordDT.GetField(stateField) == nil || oldDoc == nil {
		return contract.NewError(contract.CodeValidationFailed, "workflow state field is not defined on doctype")
	}
	currentState := fmt.Sprint(oldDoc.Get(stateField))
	if currentState == "<nil>" || currentState == "" {
		currentState = statusPolicy.InitialState
	}
	roles := actingRoles(m.op)
	if len(roles) == 0 {
		return contract.NewError(contract.CodePermissionDenied, "workflow transition requires an assigned role")
	}
	newState, newStatus, err := workflow.ApplyStatusTransition(statusPolicy, currentState, action, roles, doc) //nolint:contextcheck // the context travels in txMgr.Context
	if err != nil {
		return contract.NewError(contract.CodeValidationFailed, err.Error())
	}
	doc.Set(stateField, newState)
	doc.DocStatus = newStatus
	return nil
}

// fillItem writes the item's data into its record and binds the record to the
// operation's business scope.
func (m *mutation) fillItem(recordDT *doctype.DocType, operation string, doc, oldDoc *doctype.Document, data json.RawMessage) *contract.Error {
	if err := applyData(recordDT, doc, data, m.run.Reg, operation == "update" && !m.op.Context.AllowReadOnlyFields); err != nil {
		return contract.NewError(contract.CodeValidationFailed, err.Error())
	}
	if err := recordDT.BindBusinessScope(doc, m.op.Context.BusinessScope, operation == "create", oldDoc); err != nil {
		return contract.NewError(contract.CodePermissionDenied, err.Error())
	}
	return nil
}

// loadExisting checks that the item's data leaves the workflow status alone,
// then loads the record the item changes.
func (m *mutation) loadExisting(ctx context.Context, b *bundle, i int, recordDT *doctype.DocType, data json.RawMessage) (*doctype.Document, *contract.Error) {
	if rejectErr := m.k.rejectWorkflowStateMutation(ctx, m.run.Tx, m.op, recordDT, data); rejectErr != nil {
		return nil, contract.NewError(contract.CodeValidationFailed, rejectErr.Error())
	}
	item := b.payload.Records[i]
	return m.loadItem(item, i, recordDT, b.names[item.Key]) //nolint:contextcheck // the context travels in txMgr.Context
}
