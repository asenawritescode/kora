package ai

import (
	"context"
	"database/sql"
	"fmt"

	kdb "github.com/asenawritescode/kora/db"
)

const maxRunInventoryStatuses = 500

type RunStatusCount struct {
	Status string `json:"status"`
	Rows   int64  `json:"rows"`
}

type RunStatusInventory struct {
	TablePresent bool             `json:"table_present"`
	Statuses     []RunStatusCount `json:"statuses,omitempty"`
}

// InspectRunStatusInventory returns aggregate status counts only. It does not
// create the optional AI tables or read run IDs, messages, prompts, or outputs.
func InspectRunStatusInventory(ctx context.Context, database *sql.DB, dialect kdb.Dialect, siteID string) (RunStatusInventory, error) {
	if ctx == nil || database == nil || dialect == nil || siteID == "" {
		return RunStatusInventory{}, fmt.Errorf("AI run inventory requires context, database, dialect, and site identity")
	}
	columns, err := aiRunColumns(ctx, database, dialect, "_kora_ai_run")
	if err != nil {
		return RunStatusInventory{}, fmt.Errorf("inspect AI run schema")
	}
	if len(columns) == 0 {
		return RunStatusInventory{}, nil
	}
	for _, required := range []string{"id", "site", "status"} {
		if _, ok := columns[required]; !ok {
			return RunStatusInventory{TablePresent: true}, fmt.Errorf("unrecognized schema for _kora_ai_run")
		}
	}
	query := `SELECT status,COUNT(*) FROM _kora_ai_run WHERE site=` + dialect.Placeholder(1) + ` GROUP BY status ORDER BY status LIMIT 501`
	rows, err := database.QueryContext(ctx, query, siteID)
	if err != nil {
		return RunStatusInventory{TablePresent: true}, fmt.Errorf("aggregate AI run status counts")
	}
	defer rows.Close()
	out := RunStatusInventory{TablePresent: true, Statuses: make([]RunStatusCount, 0)}
	for rows.Next() {
		var count RunStatusCount
		if err := rows.Scan(&count.Status, &count.Rows); err != nil {
			return out, fmt.Errorf("read AI run status counts")
		}
		out.Statuses = append(out.Statuses, count)
		if len(out.Statuses) > maxRunInventoryStatuses {
			return out, fmt.Errorf("AI run inventory exceeds %d status groups", maxRunInventoryStatuses)
		}
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("read AI run status counts")
	}
	return out, nil
}

func aiRunColumns(ctx context.Context, database *sql.DB, dialect kdb.Dialect, table string) (map[string]struct{}, error) {
	var query string
	var args []any
	switch dialect.DriverName() {
	case "mysql":
		query = `SELECT column_name FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name=?`
		args = []any{table}
	case "postgres":
		query = `SELECT column_name FROM information_schema.columns WHERE table_schema=current_schema() AND table_name=$1`
		args = []any{table}
	case "libsql":
		query = `PRAGMA table_info('` + table + `')`
	default:
		return nil, fmt.Errorf("unsupported AI run inventory dialect")
	}
	rows, err := database.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns := make(map[string]struct{})
	for rows.Next() {
		var name string
		if dialect.DriverName() == "libsql" {
			var ordinal, notNull, primaryKey int
			var dataType string
			var defaultValue sql.NullString
			if err := rows.Scan(&ordinal, &name, &dataType, &notNull, &defaultValue, &primaryKey); err != nil {
				return nil, err
			}
		} else if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		columns[name] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return columns, nil
}
