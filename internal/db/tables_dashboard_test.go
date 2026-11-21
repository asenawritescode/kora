package db

import (
	"strings"
	"testing"
)

func TestSystemTableSQLIncludesDashboardOverride(t *testing.T) {
	for name, d := range map[string]Dialect{"mysql": &MySQLDialect{}, "postgres": &PostgresDialect{}} {
		t.Run(name, func(t *testing.T) {
			var ddl int
			for _, s := range d.SystemTableSQL() {
				if strings.Contains(s, "_kora_dashboard_override") {
					ddl++
				}
			}
			if ddl != 2 {
				t.Fatalf("dashboard override ddl statements = %d, want 2 (create, then the pinned column)", ddl)
			}
		})
	}
}
