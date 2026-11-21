package db

import (
	"errors"

	"github.com/go-sql-driver/mysql"
	"github.com/lib/pq"
)

// IsUniqueViolation reports whether err is a unique index violation from MySQL (error 1062) or PostgreSQL
// (SQLSTATE 23505). It classifies by driver error type, never by message text.
func IsUniqueViolation(err error) bool {
	var myErr *mysql.MySQLError
	if errors.As(err, &myErr) {
		return myErr.Number == 1062
	}
	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		return pqErr.Code == "23505"
	}
	return false
}
