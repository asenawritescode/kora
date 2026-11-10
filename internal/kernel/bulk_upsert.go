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

// bulkEnv is what every row of one upsert run shares.
type bulkEnv struct {
	siteDB *sql.DB
	reg    *doctype.Registry
	base   Operation
	opts   BulkOptions
}

// executeBulkUpsert runs every row in its own transaction: replay a stored
// receipt, else look the key up and create (0 matches), update (1) or fail (2+).
//
// Only a `unique: true` key field guarantees the key never duplicates: without
// the unique index, concurrent imports can create two records with one key, and
// later runs then report those rows as import.key_ambiguous.
func (k *Kernel) executeBulkUpsert(ctx context.Context, env bulkEnv) []BulkRowResult {
	out := make([]BulkRowResult, 0, len(env.opts.Rows))
	for _, row := range env.opts.Rows {
		res, cerr := k.upsertRow(ctx, env, row)
		if cerr == nil && res.Error != nil {
			cerr = res.Error
		}
		out = append(out, BulkRowResult{Index: row.Index, Result: res, Error: cerr})
	}
	return out
}

func (k *Kernel) upsertRow(ctx context.Context, env bulkEnv, row BulkRow) (contract.CommandResult, *contract.Error) {
	var in struct {
		Doctype string                 `json:"doctype"`
		Data    map[string]interface{} `json:"data"`
	}
	if json.Unmarshal(row.Payload, &in) != nil || in.Doctype == "" {
		return contract.CommandResult{}, contract.NewError(contract.CodeValidationFailed, "invalid import row")
	}
	dt := env.reg.Get(in.Doctype)
	if dt == nil {
		return contract.CommandResult{}, contract.NewError(contract.CodeNotFound, "unknown doctype "+in.Doctype)
	}
	key := BulkRowKey(env.opts.ImportID, row.Index)
	actorUser := env.base.Context.User
	if actorUser == "" {
		actorUser = env.base.Context.Execution.Actor().PrincipalID
	}
	for _, command := range []string{CommandRecordCreate, CommandRecordUpdate} {
		res, found, cerr := k.LookupReceiptByIntent(ctx, env.siteDB, ReceiptIntent{Execution: env.base.Context.Execution, ActorUser: actorUser, Command: command, IdempotencyKey: key})
		if cerr != nil || found {
			return res, cerr
		}
	}
	filters := make([][]any, 0, len(env.opts.KeyFields))
	for _, f := range env.opts.KeyFields {
		filters = append(filters, []any{f, "=", in.Data[f]})
	}
	rawFilters, _ := json.Marshal(filters)

	tx, err := env.siteDB.BeginTx(ctx, nil)
	if err != nil {
		return contract.CommandResult{}, contract.NewError(contract.CodeInternal, "upsert: begin transaction")
	}
	defer func() { _ = tx.Rollback() }()
	txMgr := &orm.TxManager{DB: env.siteDB, Transaction: tx, Registry: env.reg, Dialect: k.Dialect, Context: ctx,
		SiteName: env.base.Context.Site, CurrentUser: env.base.Context.User, Execution: env.base.Context.Execution, BusinessScope: env.base.Context.BusinessScope}
	names, err := txMgr.MatchingNamesLocked(dt, string(rawFilters), scopeForOperation(env.reg, env.base, dt.Name, "read"), 2) //nolint:contextcheck // the context travels in txMgr.Context
	if err != nil {
		return contract.CommandResult{}, contract.NewError(contract.CodeDependencyUnavailable, "upsert: key lookup failed")
	}
	op := env.base
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
	res, afterCommit, cerr := k.ExecuteInTx(ctx, env.siteDB, tx, env.reg, op)
	if cerr != nil || env.opts.DryRun {
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
