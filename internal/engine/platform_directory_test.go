package engine

import (
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestPlatformDirectoryStartupJoinsPingAndCloseFailures(t *testing.T) {
	database, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
	if err != nil {
		t.Fatal(err)
	}
	pingErr := errors.New("platform ping failed")
	closeErr := errors.New("platform close failed")
	mock.ExpectPing().WillReturnError(pingErr)
	mock.ExpectClose().WillReturnError(closeErr)

	_, err = openPlatformSiteDirectoryWithOpener("mysql", "discarded-test-dsn", "mysql", "", func(string, string) (*sql.DB, error) {
		return database, nil
	})
	if !errors.Is(err, pingErr) {
		t.Fatalf("startup error = %v, want ping failure", err)
	}
	if !errors.Is(err, closeErr) {
		t.Fatalf("startup error = %v, want rollback close failure", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPlatformPoolLimitConfiguration(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    int
		wantErr bool
	}{
		{name: "default", want: defaultPlatformDBMaxOpen},
		{name: "configured", value: "12", want: 12},
		{name: "zero rejected", value: "0", wantErr: true},
		{name: "invalid rejected", value: "many", wantErr: true},
		{name: "excessive rejected", value: "257", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("KORA_PLATFORM_DB_MAX_OPEN", tt.value)
			got, err := platformPoolLimitFromEnv()
			if tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), "KORA_PLATFORM_DB_MAX_OPEN") {
					t.Fatalf("error = %v, want invalid pool limit", err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("platform pool limit = %d, %v; want %d", got, err, tt.want)
			}
		})
	}
}

func TestTunePlatformDBPoolAppliesMaximumOpenConnections(t *testing.T) {
	database, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	tunePlatformDBPool(database, "postgres", 3)
	if got := database.Stats().MaxOpenConnections; got != 3 {
		t.Fatalf("platform MaxOpenConnections = %d, want 3", got)
	}
}
