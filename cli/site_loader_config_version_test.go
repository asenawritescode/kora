package cli

import (
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	kdb "github.com/asenawritescode/kora/db"
)

func TestActiveConfigMinKoraVersionRebindsPostgresAndReadsValue(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("create SQL mock:", err)
	}
	defer database.Close()
	mock.ExpectQuery(`SELECT COALESCE\(min_kora_version, ''\) FROM _kora_config_version WHERE site = \$1 AND status = 'Active' ORDER BY version DESC LIMIT 1`).
		WithArgs("tenant.example.test").
		WillReturnRows(sqlmock.NewRows([]string{"min_kora_version"}).AddRow("2.4.0"))

	got, err := activeConfigMinKoraVersion(database, &kdb.PostgresDialect{}, "tenant.example.test")
	if err != nil {
		t.Fatalf("activeConfigMinKoraVersion: %v", err)
	}
	if got != "2.4.0" {
		t.Fatalf("minimum version = %q, want 2.4.0", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("unmet SQL expectations:", err)
	}
}

func TestActiveConfigMinKoraVersionAllowsNoActiveVersionButReturnsDatabaseErrors(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("create SQL mock:", err)
	}
	defer database.Close()
	query := `SELECT COALESCE\(min_kora_version, ''\) FROM _kora_config_version WHERE site = \$1 AND status = 'Active' ORDER BY version DESC LIMIT 1`
	mock.ExpectQuery(query).WithArgs("tenant.example.test").WillReturnRows(sqlmock.NewRows([]string{"min_kora_version"}))
	if got, err := activeConfigMinKoraVersion(database, &kdb.PostgresDialect{}, "tenant.example.test"); err != nil || got != "" {
		t.Fatalf("missing active version = (%q, %v), want empty version and no error", got, err)
	}
	expectedErr := errors.New("synthetic database failure")
	mock.ExpectQuery(query).WithArgs("tenant.example.test").WillReturnError(expectedErr)
	if _, err := activeConfigMinKoraVersion(database, &kdb.PostgresDialect{}, "tenant.example.test"); !errors.Is(err, expectedErr) {
		t.Fatalf("database failure = %v, want propagated error", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("unmet SQL expectations:", err)
	}
}
