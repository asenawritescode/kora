package db

import "testing"

func TestRebindMySQLPassesThrough(t *testing.T) {
	d := &MySQLDialect{}
	q := "SELECT a FROM t WHERE b = ? AND c = ?"
	if got := Rebind(d, q); got != q {
		t.Fatalf("mysql passthrough failed: %q", got)
	}
}

func TestRebindPostgresNumbers(t *testing.T) {
	d := &PostgresDialect{}
	got := Rebind(d, "INSERT INTO t (a, b) VALUES (?, ?)")
	want := "INSERT INTO t (a, b) VALUES ($1, $2)"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestRebindPreservesStringLiterals(t *testing.T) {
	d := &PostgresDialect{}
	got := Rebind(d, `SELECT * FROM t WHERE note = 'what?' AND id = ?`)
	want := `SELECT * FROM t WHERE note = 'what?' AND id = $1`
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestRebindHandlesEmptyStringLiterals(t *testing.T) {
	d := &PostgresDialect{}
	got := Rebind(d, `SELECT * FROM t WHERE value = '' AND id = ?`)
	want := `SELECT * FROM t WHERE value = '' AND id = $1`
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
