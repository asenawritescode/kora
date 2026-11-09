package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/kernel"
	"github.com/asenawritescode/kora/internal/policy"
)

type importSpec struct {
	ImportID string `json:"import_id"`
	Target   struct {
		DocType      string   `json:"doctype"`
		OperationRef string   `json:"operation_ref"`
		KeyFields    []string `json:"key_fields"`
	} `json:"target"`
	Mapping map[string]string `json:"mapping"`
	GroupBy string            `json:"group_by"`
	DryRun  bool              `json:"dry_run"`
}

type importRowResult struct {
	Row        int             `json:"row"`
	LastRow    int             `json:"last_row,omitempty"`
	OK         bool            `json:"ok"`
	ReceiptRef string          `json:"receipt_ref,omitempty"`
	Error      *contract.Error `json:"error,omitempty"`
}

// HandleImport serves POST /api/v1/import: a create-only CSV import into a
// DocType, executed through the kernel bulk executor.
func (h *Handler) HandleImport(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, importMaxBytes+(1<<20))
	var top map[string]json.RawMessage
	specRaw := c.PostForm("spec")
	if err := json.Unmarshal([]byte(specRaw), &top); err != nil {
		badRequestError(c, "validation.invalid_json", "spec must be a JSON object", nil)
		return
	}
	for k := range top {
		switch k {
		case "import_id", "target", "mapping", "dry_run", "group_by":
		default:
			badRequestError(c, "import.unsupported", "unsupported spec key: "+k, nil)
			return
		}
	}
	var spec importSpec
	if err := json.Unmarshal([]byte(specRaw), &spec); err != nil || spec.ImportID == "" || len(spec.Mapping) == 0 {
		badRequestError(c, "import.mapping_invalid", "import_id, target.doctype and mapping are required", nil)
		return
	}
	reg := h.siteRegistry(c)
	var opTarget *importOperation
	var dt *doctype.DocType
	execution, ok := requestExecution(c)
	if spec.Target.OperationRef != "" {
		if spec.Target.DocType != "" {
			badRequestError(c, "import.target_invalid", "target takes exactly one of doctype or operation_ref", nil)
			return
		}
		if len(spec.Target.KeyFields) > 0 {
			badRequestError(c, "import.unsupported", "key_fields is not supported with operation_ref", nil)
			return
		}
		if !ok {
			writeError(c, http.StatusForbidden, "permission.denied", "verified execution is required", nil)
			return
		}
		if opTarget, ok = h.resolveImportOperation(c, reg, execution, spec.Target.OperationRef); !ok {
			return
		}
		dt = opTarget.dt
	} else {
		dt = reg.Get(spec.Target.DocType)
		if dt == nil || dt.IsChildTable {
			badRequestError(c, "import.target_invalid", "target DocType not found or is a child table", nil)
			return
		}
		// A crud-mode owned DocType is writable through normal permissions (the
		// kernel only protects commands_only ones), so importing into it is as
		// safe as a direct create. commands_only targets need a published operation.
		if dt.ModuleOwnerID != "" && dt.ModuleOwnershipMode == string(contract.MutationCommands) {
			badRequestError(c, "import.target_owned", "module-owned DocTypes cannot be imported into", map[string]any{"operations": h.ownedImportOperations(c, dt)})
			return
		}
		if !ok {
			writeError(c, http.StatusForbidden, "permission.denied", "verified execution is required", nil)
			return
		}
		scope := businessScopeFromRequest(c, execution)
		needed := []string{"create", "import"}
		if len(spec.Target.KeyFields) > 0 {
			needed = append(needed, "write")
		}
		for _, op := range needed {
			if _, err := policy.DecideForBusinessScope(execution, reg, dt.Name, op, scope, dt.BusinessScopePolicy == contract.ScopeEntityOwned); err != nil {
				writeError(c, http.StatusForbidden, "permission.denied", "cannot "+op+" on "+dt.Name, nil)
				return
			}
		}
	}
	fh, err := c.FormFile("file")
	if err != nil {
		badRequestError(c, "import.mapping_invalid", "file is required", nil)
		return
	}
	if fh.Size > importMaxBytes {
		writeError(c, http.StatusRequestEntityTooLarge, "import.too_large", "file exceeds 5MB", nil)
		return
	}
	f, err := fh.Open()
	if err != nil {
		badRequestError(c, "import.mapping_invalid", "cannot read file", nil)
		return
	}
	defer func() { _ = f.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(f, importMaxBytes+1))
	header, rows, err := parseCSVTable(raw)
	if err != nil {
		badRequestError(c, "import.mapping_invalid", err.Error(), nil)
		return
	}
	if len(rows) > importMaxRows || len(raw) > importMaxBytes {
		writeError(c, http.StatusRequestEntityTooLarge, "import.too_large", "over 5000 rows or 5MB", nil)
		return
	}
	col := map[string]int{}
	for i, name := range header {
		col[name] = i
	}
	head, child, table, merr := groupMapping(reg, dt, spec.Mapping)
	if _, ok := col[spec.GroupBy]; merr == nil && spec.GroupBy != "" && !ok {
		merr = fmt.Errorf("group_by column not in header: %s", spec.GroupBy)
	}
	if merr == nil && spec.GroupBy == "" && len(child) > 0 {
		merr = fmt.Errorf("table mappings require group_by")
	}
	for csvCol := range spec.Mapping {
		if _, ok := col[csvCol]; !ok && merr == nil {
			merr = fmt.Errorf("mapping column or field unknown: %s", csvCol)
		}
	}
	if merr != nil {
		badRequestError(c, "import.mapping_invalid", merr.Error(), nil)
		return
	}

	mapped := map[string]bool{}
	for _, field := range head {
		mapped[field] = true
	}
	for _, kf := range spec.Target.KeyFields {
		if f := dt.GetField(kf); f == nil || f.Fieldtype == "Table" || !mapped[kf] {
			badRequestError(c, "import.mapping_invalid", "key field must be a mapped, non-Table field: "+kf, nil)
			return
		}
	}

	var units []importUnit
	if spec.GroupBy != "" {
		var childDT *doctype.DocType
		if table != "" {
			childDT = reg.Get(dt.GetField(table).Options)
		}
		units = importGrouping{DT: dt, ChildDT: childDT, Table: table, Head: head, Child: child, Col: col, GroupCol: spec.GroupBy}.assemble(rows)
	} else {
		units = assembleImportRows(dt, spec.Mapping, col, rows)
	}
	if opTarget == nil {
		if err := resolveImportLinks(reg, dt, table, units, h.siteLinkLookup(c, reg)); err != nil {
			var cerr *contract.Error
			if errors.As(err, &cerr) {
				h.writeKernelError(c, cerr)
				return
			}
			writeError(c, http.StatusInternalServerError, "internal", "cannot resolve link values", nil)
			return
		}
	}
	var bulk []kernel.BulkRow
	results := map[int]importRowResult{}
	for _, u := range units {
		perr := u.Err
		for _, kf := range spec.Target.KeyFields {
			if perr == nil && u.Data[kf] == nil {
				perr = contract.NewError(contract.CodeValidationFailed, "import.key_missing: empty key "+kf)
				perr.Details, _ = json.Marshal(map[string]string{"field": kf})
			}
		}
		if perr != nil {
			results[u.First] = importRowResult{Row: u.First, LastRow: u.lastIfGrouped(spec.GroupBy), Error: perr}
			continue
		}
		if opTarget != nil {
			payload, _ := json.Marshal(u.Data)
			bulk = append(bulk, kernel.BulkRow{Index: u.First, Payload: payload})
			continue
		}
		payload, _ := json.Marshal(map[string]any{"doctype": dt.Name, "data": u.Data})
		bulk = append(bulk, kernel.BulkRow{Index: u.First, Payload: payload})
	}

	var bulkResults []kernel.BulkRowResult
	if opTarget != nil {
		// A dry run validates and authorizes only: published providers have no
		// rollback hook, so nothing is executed.
		if spec.DryRun {
			for _, r := range bulk {
				bulkResults = append(bulkResults, kernel.BulkRowResult{Index: r.Index})
			}
		} else {
			bulkResults = opTarget.run(c, execution, spec.ImportID, bulk)
		}
	} else {
		payload, _ := json.Marshal(map[string]any{"doctype": dt.Name})
		k, base, sqlDB, reg, cerr := h.buildKernelOperation(c, kernel.CommandRecordCreate, kernelRequest{Payload: payload}, false)
		if cerr != nil {
			h.writeKernelError(c, cerr)
			return
		}
		bulkResults = k.ExecuteBulk(c.Request.Context(), sqlDB, reg, base, bulk, kernel.BulkOptions{ImportID: spec.ImportID, DryRun: spec.DryRun, KeyFields: spec.Target.KeyFields})
	}
	for _, r := range bulkResults {
		rr := importRowResult{Row: r.Index, OK: r.Error == nil, Error: r.Error}
		if r.Error == nil {
			var rd kernel.ResultData
			if json.Unmarshal(r.Result.Data, &rd) == nil {
				rr.ReceiptRef, _ = rd.Document["name"].(string)
			}
			var op struct {
				ReceiptRef string `json:"receipt_ref"`
			}
			if opTarget != nil && json.Unmarshal(r.Result.Data, &op) == nil {
				rr.ReceiptRef = op.ReceiptRef
			}
		}
		results[r.Index] = rr
	}
	out, succeeded, failed := []importRowResult{}, 0, 0
	for _, u := range units {
		r := results[u.First]
		r.LastRow = u.lastIfGrouped(spec.GroupBy)
		if r.OK {
			succeeded++
			if succeeded > 20 {
				continue
			}
		} else {
			failed++
		}
		out = append(out, r)
	}
	resp := gin.H{"import_id": spec.ImportID, "total": len(units), "succeeded": succeeded, "failed": failed, "dry_run": spec.DryRun, "rows": out}
	if opTarget != nil && spec.DryRun {
		resp["executed"] = false
	}
	c.JSON(http.StatusOK, resp)
}
