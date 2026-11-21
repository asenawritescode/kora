package configstore

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
)

// ReceiptQuerier is the read surface of *sql.DB and *sql.Tx.
type ReceiptQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// OperationOwnersInFlight counts the tenant's non-terminal published operation
// receipts per owning module. A receipt is in flight while it is accepted,
// running, or its outcome is unknown. The owner is the module that provides the
// operation, the part of operation_ref before the first "~".
func OperationOwnersInFlight(ctx context.Context, q ReceiptQuerier, dialect db.QueryDialect, tenant contract.TenantID) (map[contract.ModuleID]int, error) {
	rows, err := q.QueryContext(ctx, db.Rebind(dialect, `SELECT operation_ref FROM _kora_published_operation_receipt WHERE tenant_id = ? AND state IN (?, ?, ?)`),
		string(tenant), string(contract.ReceiptAccepted), string(contract.ReceiptRunning), string(contract.ReceiptOutcomeUnknown))
	if err != nil {
		return nil, fmt.Errorf("read operations in flight: %w", err)
	}
	defer rows.Close()
	counts := make(map[contract.ModuleID]int)
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			return nil, fmt.Errorf("scan operation in flight: %w", err)
		}
		owner, _, _ := strings.Cut(ref, "~")
		counts[contract.ModuleID(owner)]++
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read operations in flight: %w", err)
	}
	return counts, nil
}
