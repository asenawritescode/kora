package api

import (
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/kernel"
	"github.com/asenawritescode/kora/policy"
)

// importOperation is a resolved, authorized, importable published operation.
type importOperation struct {
	services  SiteRuntimeService
	snapshot  contract.ModuleSnapshot
	operation registeredPublishedOperation
	scope     contract.BusinessScope
	dt        *doctype.DocType
}

// importableOperation reports whether op is a local operation backed by a
// config command of its owner that types its input with Input.Record.
func importableOperation(commands *kernel.CommandRegistry, op registeredPublishedOperation) (*kernel.CommandResource, bool) {
	if op.spec.Effect != contract.EffectLocal {
		return nil, false
	}
	cmd, ok := commands.LookupCapability(op.published.Capability, op.published.Operation)
	if !ok || cmd.ModuleOwnerID != op.owner || cmd.Input.Record == "" {
		return nil, false
	}
	return cmd, true
}

// resolveImportOperation applies the same gate as HandleExecutePublishedOperation.
// It writes the error response and returns false when the target is unusable.
func (h *Handler) resolveImportOperation(c *gin.Context, reg *doctype.Registry, execution policy.Execution, ref string) (*importOperation, bool) {
	scope := businessScopeFromRequest(c, execution)
	services := h.runtimeService(c, c.GetString("site_name"))
	snapshot, op, ok := publishedOperationByRef(services, ref)
	if !ok {
		badRequestError(c, "import.target_unsupported", "operation is not a registered published operation", nil)
		return nil, false
	}
	if contract.ValidateResourceBusinessScope(scope, op.published.ScopePolicy) != nil {
		writeError(c, http.StatusForbidden, "permission.denied", "Published operation scope is not permitted", nil)
		return nil, false
	}
	if _, authErr := authorizePublishedOperation(execution, reg, op.owner, op.published, scope); authErr != nil {
		writeError(c, http.StatusForbidden, "permission.denied", "Published operation access is not granted", nil)
		return nil, false
	}
	cmd, ok := importableOperation(h.kernelCommands(services), op)
	var dt *doctype.DocType
	if ok {
		dt = reg.Get(cmd.Input.Record)
	}
	if dt == nil || dt.IsChildTable {
		badRequestError(c, "import.target_unsupported", "operation is not a local config command with a record input", nil)
		return nil, false
	}
	if services.ExecutePublishedOperation == nil {
		writeError(c, http.StatusServiceUnavailable, "module.operation_unavailable", "Published operation executor is unavailable", nil)
		return nil, false
	}
	return &importOperation{services: services, snapshot: snapshot, operation: op, scope: scope, dt: dt}, true
}

// ownedImportOperations lists the importable operation refs of dt's owner.
func (h *Handler) ownedImportOperations(c *gin.Context, dt *doctype.DocType) []string {
	services := h.runtimeService(c, c.GetString("site_name"))
	refs := []string{}
	if services.LoadModuleSnapshot == nil {
		return refs
	}
	snapshot, err := services.LoadModuleSnapshot()
	if err != nil {
		return refs
	}
	for _, module := range snapshot.Modules {
		if string(module.ID) != dt.ModuleOwnerID {
			continue
		}
		for _, published := range module.PublishedOperations {
			ref := contract.PublishedOperationRef(module.ID, published.Capability, published.Operation)
			_, op, ok := publishedOperationByRef(services, ref)
			if !ok {
				continue
			}
			if cmd, ok := importableOperation(h.kernelCommands(services), op); ok && cmd.Input.Record == dt.Name {
				refs = append(refs, ref)
			}
		}
	}
	return refs
}

// run executes each unit as one published-operation call keyed by BulkRowKey.
func (o *importOperation) run(c *gin.Context, execution policy.Execution, importID string, rows []kernel.BulkRow) []kernel.BulkRowResult {
	out := make([]kernel.BulkRowResult, 0, len(rows))
	kernel.SequenceBulk(importID, rows, func(row kernel.BulkRow, key string) {
		res := kernel.BulkRowResult{Index: row.Index}
		receipt, err := o.services.ExecutePublishedOperation(c.Request.Context(), execution, o.scope, o.operation.owner, o.operation.published, o.operation.spec.Input, contract.CommandMeta{IdempotencyKey: contract.IdempotencyKey(key)}, row.Payload)
		switch {
		case err != nil:
			res.Error = asImportError(err)
		case validatePublishedOperationReceipt(receipt, o.operation, o.scope) != nil:
			res.Error = contract.NewError(contract.CodeInternal, "published operation returned an invalid receipt")
		case receipt.Status != contract.ReceiptCompleted:
			res.Error = receipt.Error
			if res.Error == nil {
				res.Error = contract.NewError(contract.CodeConflict, "operation receipt status "+string(receipt.Status))
			}
		default:
			res.Result.Data, _ = json.Marshal(map[string]string{"receipt_ref": string(receipt.ExecutionID)})
		}
		out = append(out, res)
	})
	return out
}

func asImportError(err error) *contract.Error {
	if typed, ok := err.(*contract.Error); ok && typed != nil {
		return typed
	}
	return contract.NewError(contract.CodeDependencyUnavailable, "published operation could not be completed")
}
