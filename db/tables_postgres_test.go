package db

import (
	"strings"
	"testing"
)

func TestPostgresSystemDDLIsNotMySQLSpecific(t *testing.T) {
	for group, statements := range map[string][]string{
		"extensibility": ExtensibilityTablesPostgres(),
		"conversation":  ConversationTablesPostgres(),
	} {
		if len(statements) == 0 {
			t.Fatalf("%s PostgreSQL DDL is empty", group)
		}
		for index, statement := range statements {
			upper := strings.ToUpper(statement)
			for _, unsupported := range []string{"ENGINE=INNODB", "ON UPDATE CURRENT_TIMESTAMP", "TINYINT", "DATETIME(", "UNIQUE KEY", "KEY IDX_"} {
				if strings.Contains(upper, unsupported) {
					t.Errorf("%s DDL %d contains MySQL-only token %q", group, index, unsupported)
				}
			}
		}
	}
}
