package db

import (
	"errors"
	"testing"

	"github.com/go-sql-driver/mysql"
	"github.com/lib/pq"
)

func TestIsUniqueViolationClassifiesDuplicateKeysOnly(t *testing.T) {
	tests := []struct {
		name    string
		dialect Dialect
		err     error
		want    bool
	}{
		{"mysql duplicate entry", &MySQLDialect{}, &mysql.MySQLError{Number: 1062}, true},
		{"mysql lock wait is not a duplicate", &MySQLDialect{}, &mysql.MySQLError{Number: 1205}, false},
		{"mysql wrapped duplicate", &MySQLDialect{}, errors.Join(errors.New("insert"), &mysql.MySQLError{Number: 1062}), true},
		{"postgres unique violation", &PostgresDialect{}, &pq.Error{Code: "23505"}, true},
		{"postgres serialization failure is not a duplicate", &PostgresDialect{}, &pq.Error{Code: "40001"}, false},
		{"libsql unique constraint", &LibSQLDialect{}, errors.New("UNIQUE constraint failed: _kora_capability_lease.lease_id"), true},
		{"libsql other failure", &LibSQLDialect{}, errors.New("database is locked"), false},
		{"nil error", &PostgresDialect{}, nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.dialect.IsUniqueViolation(tc.err); got != tc.want {
				t.Fatalf("IsUniqueViolation(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
