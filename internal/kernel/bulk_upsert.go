package kernel

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/orm"
)

// ErrKeyAmbiguous is the per-row message when a key matches two or more records.
const ErrKeyAmbiguous = "import.key_ambiguous"

// executeBulkUpsert runs every row in its own transaction: replay a stored
// receipt, else look the key up and create (0 matches), update (1) or fail (2+).
//
// Only a `unique: true` key field guarantees the key never duplicates: without
// the unique index, concurrent imports can create two records with one key, and
// later runs then report those rows as import.key_ambiguous.
func (k *Kernel) executeBulkUpsert(ctx context.Context, siteDB *sql.DB, reg *doctype.Registry, base Operation, rows []BulkRow, opts BulkOptions) []BulkRowResult {
	out := make([]BulkRowResult, 0, len(rows))
	for _, row := range rows {
		res, cerr := k.upsertRow(ctx, siteDB, reg, base, row, opts)
		if cerr == nil && res.Error != nil {
			cerr = res.Error
		}
		out = append(out, BulkRowResult{Index: row.Index, Result: res, Error: cerr})
	}
	return out
}

func (k *Kernel) upsertRow(ctx context.Context, siteDB *sql.DB, reg *doctype.Registry, base Operation, row BulkRow, opts BulkOptions) (contract.CommandResult, *contract.Error) {
	var in struct {
		Doctype string                 `json:"doctype"`
		Data    map[string]interface{} `json:"data"`
	}
	if json.Unmarshal(row.Payload, &in) != nil || in.Doctype == "" {
		return contract.CommandResult{}, contract.NewError(contract.CodeValidationFailed, "invalid import row")
	}
	dt := reg.Get(in.Doctype)
	if dt == nil {
		return contract.CommandResult{}, contract.NewError(contract.CodeNotFound, "unknown doctype "+in.Doctype)
	}
	key := BulkRowKey(opts.ImportID, row.Index)
	actorUser := base.Context.User
	if actorUser == "" {
		actorUser = base.Context.Execution.Actor().PrincipalID
	}
	for _, command := range []string{CommandRecordCreate, CommandRecordUpdate} {
		res, found, cerr := k.LookupReceiptByIntent(ctx, siteDB, base.Context.Execution, actorUser, command, key)
		if cerr != nil || found {
			return res, cerr
		}
	}
	filters := make([][]any, 0, len(opts.KeyFields))
	for _, f := range opts.KeyFields {
		filters = append(filters, []any{f, "=", in.Data[f]})
	}
	rawFilters, _ := json.Marshal(filters)

	tx, err := siteDB.BeginTx(ctx, nil)
	if err != nil {
		return contract.CommandResult{}, contract.NewError(contract.CodeInternal, "upsert: begin transaction")
	}
	defer func() { _ = tx.Rollback() }()
	txMgr := &orm.TxManager{DB: siteDB, Transaction: tx, Registry: reg, Dialect: k.Dialect, Context: ctx,
		SiteName: base.Context.Site, CurrentUser: base.Context.User, Execution: base.Context.Execution, BusinessScope: base.Context.BusinessScope}
	names, err := txMgr.MatchingNamesLocked(dt, string(rawFilters), scopeForOperation(reg, base, dt.Name, "read"), 2)
	if err != nil {
		return contract.CommandResult{}, contract.NewError(contract.CodeDependencyUnavailable, "upsert: key lookup failed")
	}
	op := base
	op.Payload = row.Payload
	op.Context.IdempotencyKey = key
	switch len(names) {
	case 0:
		op.Command = CommandRecordCreate
	case 1:
		op.Command = CommandRecordUpdate
		op.Payload, _ = json.Marshal(RecordUpdatePayload{Doctype: dt.Name, Name: names[0], Data: mustJSON(in.Data)})
	default:
		e := contract.NewError(contract.CodeConflict, fmt.Sprintf("%s: %d %s records match this key", ErrKeyAmbiguous, len(names), dt.Name))
		e.Details, _ = json.Marshal(map[string]string{"code": ErrKeyAmbiguous})
		return contract.CommandResult{}, e
	}
	res, afterCommit, cerr := k.ExecuteInTx(ctx, siteDB, tx, reg, op)
	if cerr != nil || opts.DryRun {
		return res, cerr
	}
	if err := tx.Commit(); err != nil {
		return contract.CommandResult{}, contract.NewError(contract.CodeConflict, "upsert: commit failed (concurrent change)")
	}
	if afterCommit != nil {
		afterCommit()
	}
	return res, nil
}

func mustJSON(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
