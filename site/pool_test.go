package site

import (
	"context"
	"database/sql"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestMySQLTenantPoolRetainsOnlyOneIdleConnection(t *testing.T) {
	database, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	tuneConnectionPool(database, "mysql", 0, 0)
	if got := database.Stats().MaxOpenConnections; got != 25 {
		t.Fatalf("MySQL tenant max-open = %d, want 25", got)
	}
	connections := make([]*sql.Conn, 3)
	for i := range connections {
		conn, err := database.Conn(context.Background())
		if err != nil {
			t.Fatalf("open concurrent tenant connection %d: %v", i, err)
		}
		connections[i] = conn
	}
	for _, conn := range connections {
		if err := conn.Close(); err != nil {
			t.Fatal("release tenant connection:", err)
		}
	}
	stats := database.Stats()
	if stats.OpenConnections != 1 || stats.Idle != 1 {
		t.Fatalf("MySQL tenant pool retained open=%d idle=%d; want one idle socket", stats.OpenConnections, stats.Idle)
	}
}

func TestTenantPoolHonorsConfiguredConnectionBounds(t *testing.T) {
	database, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	tuneConnectionPool(database, "mysql", 3, 0)
	if got := database.Stats().MaxOpenConnections; got != 3 {
		t.Fatalf("configured MySQL tenant max-open = %d, want 3", got)
	}
	if got := database.Stats().Idle; got != 0 {
		t.Fatalf("configured MySQL tenant max-idle should be zero; idle=%d", got)
	}

}
