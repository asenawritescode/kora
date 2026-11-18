package db

import "testing"

func TestORMFilledDefault(t *testing.T) {
	tests := []struct {
		name      string
		fieldtype string
		value     string
		want      bool
	}{
		{"date today", "Date", "today", true},
		{"date today mixed case", "Date", " Today ", true},
		{"datetime now", "Datetime", "Now", true},
		{"datetime today", "Datetime", "today", true},
		{"date literal", "Date", "2024-01-01", false},
		{"time now stays in db", "Time", "now", false},
		{"data today is text", "Data", "today", false},
		{"empty", "Date", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ORMFilledDefault(tt.fieldtype, tt.value); got != tt.want {
				t.Errorf("ORMFilledDefault(%q, %q) = %v, want %v", tt.fieldtype, tt.value, got, tt.want)
			}
		})
	}
}

func TestSQLDefaultClauseORMFilled(t *testing.T) {
	tests := []struct {
		name, dialect, fieldtype, value, want string
	}{
		{"mysql date today has no db default", "mysql", "Date", "today", ""},
		{"postgres date today has no db default", "postgres", "Date", "today", ""},
		{"mysql datetime now has no db default", "mysql", "Datetime", "now", ""},
		{"postgres datetime now has no db default", "postgres", "Datetime", "now", ""},
		{"mysql time now keeps expression", "mysql", "Time", "now", " DEFAULT CURRENT_TIME(6)"},
		{"libsql date today keeps expression", "libsql", "Date", "today", " DEFAULT (DATE('now'))"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sqlDefaultClause(tt.dialect, tt.fieldtype, tt.value); got != tt.want {
				t.Errorf("sqlDefaultClause(%s, %s, %q) = %q, want %q", tt.dialect, tt.fieldtype, tt.value, got, tt.want)
			}
		})
	}
}
