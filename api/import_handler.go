package api

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/kernel"
	"github.com/asenawritescode/kora/policy"
)

type importSpec struct {
	ImportID string `json:"import_id"`
	Target   struct {
		DocType string `json:"doctype"`
	} `json:"target"`
	Mapping map[string]string `json:"mapping"`
	DryRun  bool              `json:"dry_run"`
}

type importRowResult struct {
	Row        int             `json:"row"`
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
		case "import_id", "target", "mapping", "dry_run":
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
	dt := reg.Get(spec.Target.DocType)
	if dt == nil || dt.IsChildTable {
		badRequestError(c, "import.target_invalid", "target DocType not found or is a child table", nil)
		return
	}
	if dt.ModuleOwnerID != "" {
		badRequestError(c, "import.target_owned", "module-owned DocTypes cannot be imported into", nil)
		return
	}
	execution, ok := requestExecution(c)
	if !ok {
		writeError(c, http.StatusForbidden, "permission.denied", "verified execution is required", nil)
		return
	}
	scope := businessScopeFromRequest(c, execution)
	for _, op := range []string{"create", "import"} {
		if _, err := policy.DecideForBusinessScope(execution, reg, dt.Name, op, scope, dt.BusinessScopePolicy == contract.ScopeEntityOwned); err != nil {
			writeError(c, http.StatusForbidden, "permission.denied", "cannot "+op+" on "+dt.Name, nil)
			return
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
	header, table, err := parseCSVTable(raw)
	if err != nil {
		badRequestError(c, "import.mapping_invalid", err.Error(), nil)
		return
	}
	if len(table) > importMaxRows || len(raw) > importMaxBytes {
		writeError(c, http.StatusRequestEntityTooLarge, "import.too_large", "over 5000 rows or 5MB", nil)
		return
	}
	col := map[string]int{}
	for i, name := range header {
		col[name] = i
	}
	for csvCol, field := range spec.Mapping {
		if _, ok := col[csvCol]; !ok || dt.GetField(field) == nil {
			badRequestError(c, "import.mapping_invalid", "mapping column or field unknown: "+csvCol, nil)
			return
		}
	}

	var bulk []kernel.BulkRow
	results := map[int]importRowResult{}
	for i, rec := range table {
		idx := i + 1
		data := map[string]any{}
		var perr *contract.Error
		for csvCol, field := range spec.Mapping {
			var cell string
			if ci := col[csvCol]; ci < len(rec) {
				cell = rec[ci]
			}
			v, e := parseImportValue(dt.GetField(field), cell)
			if e != nil {
				perr = contract.NewError(contract.CodeValidationFailed, e.Error())
				perr.Details, _ = json.Marshal(map[string]string{"field": field})
				break
			}
			if v != nil {
				data[field] = v
			}
		}
		if perr != nil {
			results[idx] = importRowResult{Row: idx, Error: perr}
			continue
		}
		payload, _ := json.Marshal(map[string]any{"doctype": dt.Name, "data": data})
		bulk = append(bulk, kernel.BulkRow{Index: idx, Payload: payload})
	}

	payload, _ := json.Marshal(map[string]any{"doctype": dt.Name})
	k, base, sqlDB, reg, cerr := h.buildKernelOperation(c, kernel.CommandRecordCreate, kernelRequest{Payload: payload}, false)
	if cerr != nil {
		h.writeKernelError(c, cerr)
		return
	}
	for _, r := range k.ExecuteBulk(c.Request.Context(), sqlDB, reg, base, bulk, kernel.BulkOptions{ImportID: spec.ImportID, DryRun: spec.DryRun}) {
		rr := importRowResult{Row: r.Index, OK: r.Error == nil, Error: r.Error}
		if r.Error == nil {
			var rd kernel.ResultData
			if json.Unmarshal(r.Result.Data, &rd) == nil {
				rr.ReceiptRef, _ = rd.Document["name"].(string)
			}
		}
		results[r.Index] = rr
	}
	out, succeeded, failed := []importRowResult{}, 0, 0
	for idx := 1; idx <= len(table); idx++ {
		r := results[idx]
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
	c.JSON(http.StatusOK, gin.H{"import_id": spec.ImportID, "total": len(table), "succeeded": succeeded, "failed": failed, "dry_run": spec.DryRun, "rows": out})
}
