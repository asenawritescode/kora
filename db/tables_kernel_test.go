package db

import (
	"strings"
	"testing"
)

func TestKernelReceiptDDLStoresReplayableResultForEveryDialect(t *testing.T) {
	tests := []struct {
		name string
		ddl  []string
	}{
		{name: "mysql", ddl: KernelTablesMySQL()},
		{name: "libsql", ddl: KernelTablesLibSQL()},
		{name: "postgres", ddl: KernelTablesPostgres()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			joined := strings.Join(test.ddl, "\n")
			if !strings.Contains(joined, "result_json") {
				t.Fatal("idempotency receipt schema must persist the original command result")
			}
		})
	}
}

func TestRecordHistoryDDLExistsForEveryDialect(t *testing.T) {
	for _, test := range []struct {
		name string
		ddl  []string
	}{
		{name: "mysql", ddl: KernelTablesMySQL()},
		{name: "libsql", ddl: KernelTablesLibSQL()},
		{name: "postgres", ddl: KernelTablesPostgres()},
	} {
		t.Run(test.name, func(t *testing.T) {
			joined := strings.Join(test.ddl, "\n")
			if !strings.Contains(joined, "_kora_record_history") || !strings.Contains(joined, "changes_json") {
				t.Fatal("record history schema is missing")
			}
		})
	}
}
