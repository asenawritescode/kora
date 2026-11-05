package kernel

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/doctype"
)

// BulkRow is one input of a bulk run; Index is the caller's row number.
type BulkRow struct {
	Index   int
	Payload json.RawMessage
}

// BulkRowResult reports the outcome of one row.
type BulkRowResult struct {
	Index  int
	Result contract.CommandResult
	Error  *contract.Error // nil when the row succeeded
}

// BulkOptions controls a bulk run.
type BulkOptions struct {
	// ImportID scopes per-row idempotency keys: sha256(ImportID:index).
	ImportID string
	// DryRun executes each row and rolls it back; nothing is persisted.
	DryRun bool
	// KeyFields switches to upsert: a row updates the single record matching
	// its key values, or creates one when none matches.
	KeyFields []string
}

// BulkRowKey returns the idempotency key for a row of a bulk run.
func BulkRowKey(importID string, index int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", importID, index)))
	return hex.EncodeToString(sum[:])
}

// ExecuteBulk runs base.Command once per row, sequentially, one transaction per
// row. It never stops on failure: every row is reported. Each row carries its own
// idempotency key so re-running a bulk skips completed rows (replayed receipts).
func (k *Kernel) ExecuteBulk(ctx context.Context, siteDB *sql.DB, reg *doctype.Registry, base Operation, rows []BulkRow, opts BulkOptions) []BulkRowResult {
	if len(opts.KeyFields) > 0 {
		return k.executeBulkUpsert(ctx, siteDB, reg, base, rows, opts)
	}
	exec := func(op Operation) (contract.CommandResult, *contract.Error) {
		if opts.DryRun {
			return k.dryRun(ctx, siteDB, reg, op)
		}
		return k.Execute(ctx, siteDB, reg, op)
	}
	return runBulk(base, rows, opts, exec)
}

// SequenceBulk visits rows in order, never stopping, with each row's
// idempotency key. runBulk and published-operation imports share it.
func SequenceBulk(importID string, rows []BulkRow, fn func(row BulkRow, key string)) {
	for _, row := range rows {
		fn(row, BulkRowKey(importID, row.Index))
	}
}

func runBulk(base Operation, rows []BulkRow, opts BulkOptions, exec func(Operation) (contract.CommandResult, *contract.Error)) []BulkRowResult {
	out := make([]BulkRowResult, 0, len(rows))
	for _, row := range rows {
		op := base
		op.Payload = row.Payload
		op.Context.IdempotencyKey = BulkRowKey(opts.ImportID, row.Index)
		res, err := exec(op)
		if err == nil && res.Error != nil {
			err = res.Error
		}
		out = append(out, BulkRowResult{Index: row.Index, Result: res, Error: err})
	}
	return out
}

func (k *Kernel) dryRun(ctx context.Context, siteDB *sql.DB, reg *doctype.Registry, op Operation) (contract.CommandResult, *contract.Error) {
	tx, err := siteDB.BeginTx(ctx, nil)
	if err != nil {
		return contract.CommandResult{}, contract.NewError(contract.CodeInternal, "dry run: begin transaction")
	}
	defer func() { _ = tx.Rollback() }()
	res, _, cerr := k.ExecuteInTx(ctx, siteDB, tx, reg, op)
	return res, cerr
}
