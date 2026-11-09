package db

import (
	"fmt"
	"strings"
)

func sqlDefaultClause(dialect string, fieldtype string, value string) string {
	if value == "" || fieldtype == "Check" {
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
		if normalizedValue != "today" {
			return "", false
		}
		switch dialect {
		case "mysql", "postgres":
			return "CURRENT_DATE", true
		case "libsql":
			return "(DATE('now'))", true
		}
	case "Datetime":
		if normalizedValue != "now" && normalizedValue != "today" {
			return "", false
		}
		switch dialect {
		case "mysql":
			return "CURRENT_TIMESTAMP(6)", true
		case "postgres":
			return "NOW()", true
		case "libsql":
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
