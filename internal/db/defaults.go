package db

import (
	"fmt"
	"strings"
)

// ORMFilledDefault reports whether a field default is applied by the ORM on
// insert (Date "today"; Datetime "now" or "today") instead of by the database.
// MySQL and Postgres get no DDL default for these: the tenant business clock
// is the source of truth, and the server zone must not decide the day.
func ORMFilledDefault(fieldtype string, value string) bool {
	normalizedValue := strings.ToLower(strings.TrimSpace(value))
	switch strings.TrimSpace(fieldtype) {
	case "Date":
		return normalizedValue == "today"
	case "Datetime":
		return normalizedValue == "now" || normalizedValue == "today"
	}
	return false
}

func sqlDefaultClause(dialect string, fieldtype string, value string) string {
	if value == "" || fieldtype == "Check" {
		return ""
	}

	if dialect != "libsql" && ORMFilledDefault(fieldtype, value) {
		return ""
	}

	if dialect == "mysql" && !mysqlSupportsDefault(fieldtype) {
		return ""
	}

	if expr, ok := temporalDefaultExpr(dialect, fieldtype, value); ok {
		return " DEFAULT " + expr
	}

	return fmt.Sprintf(" DEFAULT '%s'", escapeSQLDefault(value))
}

func mysqlSupportsDefault(fieldtype string) bool {
	switch fieldtype {
	case "Text", "Text Editor", "Attach", "Attach Image", "Attach Audio", "JSON":
		return false
	default:
		return true
	}
}

func temporalDefaultExpr(dialect string, fieldtype string, value string) (string, bool) {
	normalizedType := strings.TrimSpace(fieldtype)
	normalizedValue := strings.ToLower(strings.TrimSpace(value))

	switch normalizedType {
	case "Date":
		// libsql only: MySQL and Postgres never reach here (ORMFilledDefault).
		if normalizedValue == "today" && dialect == "libsql" {
			return "(DATE('now'))", true
		}
	case "Datetime":
		if dialect == "libsql" && (normalizedValue == "now" || normalizedValue == "today") {
			return "(STRFTIME('%Y-%m-%d %H:%M:%f', 'NOW'))", true
		}
	case "Time":
		if normalizedValue != "now" {
			return "", false
		}
		switch dialect {
		case "mysql":
			return "CURRENT_TIME(6)", true
		case "postgres":
			return "CURRENT_TIME", true
		case "libsql":
			return "(TIME('now'))", true
		}
	}

	return "", false
}

func escapeSQLDefault(value string) string {
	return strings.ReplaceAll(value, "'", "''")
}
