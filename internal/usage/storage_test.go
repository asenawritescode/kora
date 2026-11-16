package usage

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/asenawritescode/kora/internal/db"
	_ "github.com/mattn/go-sqlite3"
)

// The LibSQL dialect runs in-process on SQLite, so this test needs no server.
func TestStorageBytesLibSQL(t *testing.T) {
	database, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "storage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	dialect := db.Resolve("libsql")
	if _, err = database.Exec("CREATE TABLE storage_rows (name TEXT)"); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		rows int
	}{
		{name: "table without rows", rows: 0},
		{name: "table with rows", rows: 50},
	}
	inserted := 0
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for ; inserted < tt.rows; inserted++ {
				if _, err = database.Exec("INSERT INTO storage_rows (name) VALUES ('row')"); err != nil {
					t.Fatal(err)
				}
			}
			got, err := Storage{DB: database, Dialect: dialect}.Bytes(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if got <= 0 {
				t.Fatalf("database size = %d bytes, want > 0", got)
			}
		})
	}
}
