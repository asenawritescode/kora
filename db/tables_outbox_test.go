package db

import (
	"strings"
	"testing"
)

func TestOutboxReceiptDDLStoresOptionalSourceSequenceForEachDialect(t *testing.T) {
	for _, tc := range []struct {
		name string
		ddl  []string
	}{
		{name: "mysql", ddl: OutboxTablesMySQL()},
		{name: "postgres", ddl: OutboxTablesPostgres()},
		{name: "libsql", ddl: OutboxTablesLibSQL()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, statement := range tc.ddl {
				if !strings.Contains(statement, "_kora_outbox_receipt") {
					continue
				}
				if !strings.Contains(statement, "source_sequence") {
					t.Fatalf("%s receipt DDL lacks source_sequence: %s", tc.name, statement)
				}
				return
			}
			t.Fatalf("%s outbox DDL lacks the consumer receipt table", tc.name)
		})
	}
}
