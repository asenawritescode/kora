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
