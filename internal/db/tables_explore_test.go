package db

import (
	"strings"
	"testing"
)

func TestSystemTableSQLIncludesSavedReport(t *testing.T) {
	for name, d := range map[string]Dialect{"mysql": &MySQLDialect{}, "postgres": &PostgresDialect{}} {
		t.Run(name, func(t *testing.T) {
			var ddl int
			for _, s := range d.SystemTableSQL() {
				if strings.Contains(s, "_kora_saved_report (") {
					ddl++
				}
			}
			if ddl != 1 {
				t.Fatalf("saved report ddl statements = %d, want 1", ddl)
			}
		})
	}
}
