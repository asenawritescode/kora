package db

import (
	"strings"
	"testing"
)

func TestSystemTableSQLIncludesPaymentIntent(t *testing.T) {
	for name, d := range map[string]Dialect{"mysql": &MySQLDialect{}, "libsql": &LibSQLDialect{}, "postgres": &PostgresDialect{}} {
		t.Run(name, func(t *testing.T) {
			var ddl, uniq int
			for _, s := range d.SystemTableSQL() {
				if strings.Contains(s, "_kora_payment_intent") {
					ddl++
				}
				if strings.Contains(s, "UNIQUE") && strings.Contains(s, "idempotency_key") {
					uniq++
				}
			}
			if ddl < 1 || uniq != 1 {
				t.Fatalf("payment ddl=%d idempotency unique=%d", ddl, uniq)
			}
		})
	}
}
