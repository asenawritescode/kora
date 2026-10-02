package ai

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	kdb "github.com/asenawritescode/kora/db"
)

type dialectContextKey struct{}

type sqlTime struct {
	Time  time.Time
	Valid bool
}

func (t *sqlTime) Scan(value any) error {
	if value == nil {
		t.Time = time.Time{}
		t.Valid = false
		return nil
	}
	var parsed time.Time
	switch value := value.(type) {
	case time.Time:
		parsed = value
	case string:
		var err error
		parsed, err = parseSQLTime(value)
		if err != nil {
			return err
		}
	case []byte:
		var err error
		parsed, err = parseSQLTime(string(value))
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("cannot scan %T into AI timestamp", value)
	}
	t.Time = parsed
	t.Valid = true
	return nil
}

func parseSQLTime(value string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999-07:00", "2006-01-02 15:04:05.999999999", "2006-01-02 15:04:05"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid AI timestamp %q", value)
}

// WithDialect binds AI persistence to the dialect of the current tenant.
func WithDialect(ctx context.Context, dialect kdb.Dialect) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, dialectContextKey{}, dialect)
}

func dialectFromContext(ctx context.Context) kdb.Dialect {
	if ctx != nil {
		if dialect, ok := ctx.Value(dialectContextKey{}).(kdb.Dialect); ok && dialect != nil {
			return dialect
		}
	}
	return kdb.Resolve(os.Getenv("KORA_DB_TYPE"))
}

var mysqlExcludedColumn = regexp.MustCompile(`excluded\.([a-zA-Z0-9_]+)`)

func bindAIQuery(ctx context.Context, query string) string {
	dialect := dialectFromContext(ctx)
	if dialect.DriverName() == "mysql" {
		query = strings.Replace(query, "ON CONFLICT(id) DO UPDATE SET", "ON DUPLICATE KEY UPDATE", 1)
		query = mysqlExcludedColumn.ReplaceAllStringFunc(query, func(match string) string {
			column := mysqlExcludedColumn.FindStringSubmatch(match)[1]
			return "VALUES(" + column + ")"
		})
	}
	return kdb.Rebind(dialect, query)
}

type aiSQL struct{ db *sql.DB }

func (q aiSQL) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return q.db.ExecContext(ctx, bindAIQuery(ctx, query), args...)
}

func (q aiSQL) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return q.db.QueryContext(ctx, bindAIQuery(ctx, query), args...)
}

func (q aiSQL) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return q.db.QueryRowContext(ctx, bindAIQuery(ctx, query), args...)
}
