package ai

import (
	"context"
	"testing"

	"github.com/asenawritescode/kora/db"
)

func TestBindAIQueryUsesTenantDialect(t *testing.T) {
	query := `INSERT INTO t (id, value) VALUES (?, ?) ON CONFLICT(id) DO UPDATE SET value=excluded.value`
	tests := []struct {
		name    string
		dialect string
		want    string
	}{
		{"postgres", "postgres", `INSERT INTO t (id, value) VALUES ($1, $2) ON CONFLICT(id) DO UPDATE SET value=excluded.value`},
		{"mysql", "mysql", `INSERT INTO t (id, value) VALUES (?, ?) ON DUPLICATE KEY UPDATE value=VALUES(value)`},
		{"libsql", "libsql", query},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := WithDialect(context.Background(), db.Resolve(tt.dialect))
			if got := bindAIQuery(ctx, query); got != tt.want {
				t.Fatalf("bound query = %q, want %q", got, tt.want)
			}
		})
	}
}
