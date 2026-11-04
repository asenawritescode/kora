package configstore

import (
	"context"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/db"
)

func TestConfigRevisionSchemaSupportsEveryDialect(t *testing.T) {
	for _, driver := range []string{"mysql", "postgres", "libsql"} {
		t.Run(driver, func(t *testing.T) {
			statements := ConfigRevisionSchema(db.Resolve(driver))
			if len(statements) != 1 || !containsAll(statements[0], "_kora_config_revision", "revision BIGINT", "CHECK (revision >= 0)") {
				t.Fatalf("config revision schema for %s: %#v", driver, statements)
			}
		})
	}
}

func TestSeedAndReadConfigRevision(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("create SQL mock:", err)
	}
	defer database.Close()
	dialect := db.MySQL()
	mock.ExpectExec("INSERT IGNORE INTO _kora_config_revision \\(id, revision\\) VALUES \\(1, 1\\)").
		WillReturnResult(sqlmock.NewResult(0, 0))
	if err := SeedConfigRevision(context.Background(), database, dialect); err != nil {
		t.Fatal("seed config revision:", err)
	}
	mock.ExpectQuery("SELECT revision FROM _kora_config_revision WHERE id = 1").
		WillReturnRows(sqlmock.NewRows([]string{"revision"}).AddRow(int64(17)))
	revision, err := CurrentConfigRevision(context.Background(), database, dialect)
	if err != nil || revision != 17 {
		t.Fatalf("CurrentConfigRevision() = %d, %v; want 17", revision, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("SQL expectations:", err)
	}
}

func TestSeedConfigRevisionIsIdempotentForPostgresMigrationRetry(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("create SQL mock:", err)
	}
	defer database.Close()
	dialect := db.Resolve("postgres")
	query := `INSERT INTO _kora_config_revision (id, revision) VALUES (1, 1) ON CONFLICT (id) DO NOTHING`
	mock.ExpectExec(regexp.QuoteMeta(query)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(query)).WillReturnResult(sqlmock.NewResult(0, 0))
	for i := 0; i < 2; i++ {
		if err := SeedConfigRevision(context.Background(), database, dialect); err != nil {
			t.Fatalf("seed attempt %d: %v", i+1, err)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("SQL expectations:", err)
	}
}

func TestBumpConfigRevisionSharesMutationTransactionAndFailsClosed(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("create SQL mock:", err)
	}
	defer database.Close()
	mock.ExpectBegin()
	tx, err := database.Begin()
	if err != nil {
		t.Fatal("begin transaction:", err)
	}
	mock.ExpectExec(`UPDATE _kora_config_revision SET revision = revision \+ 1 WHERE id = 1 AND revision < 9223372036854775807`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := BumpConfigRevisionTx(tx, db.MySQL()); err != nil {
		t.Fatal("bump config revision:", err)
	}
	mock.ExpectCommit()
	if err := tx.Commit(); err != nil {
		t.Fatal("commit mutation:", err)
	}
	mock.ExpectBegin()
	tx, err = database.Begin()
	if err != nil {
		t.Fatal("begin missing-row transaction:", err)
	}
	mock.ExpectExec(`UPDATE _kora_config_revision SET revision = revision \+ 1 WHERE id = 1 AND revision < 9223372036854775807`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectRollback()
	if err := BumpConfigRevisionTx(tx, db.MySQL()); err == nil {
		t.Fatal("missing revision row was accepted")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal("rollback missing-row transaction:", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("SQL expectations:", err)
	}
}

func TestBumpConfigRevisionAtTxCompareAndSet(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("create SQL mock:", err)
	}
	defer database.Close()
	dialect := db.Resolve("postgres")
	query := `UPDATE _kora_config_revision SET revision = revision + 1 WHERE id = 1 AND revision = $1 AND revision < 9223372036854775807`
	mock.ExpectBegin()
	tx, err := database.Begin()
	if err != nil {
		t.Fatal("begin transaction:", err)
	}
	mock.ExpectExec(regexp.QuoteMeta(query)).WithArgs(int64(7)).WillReturnResult(sqlmock.NewResult(0, 1))
	if err := BumpConfigRevisionAtTx(tx, dialect, 7); err != nil {
		t.Fatal("conditional bump:", err)
	}
	mock.ExpectCommit()
	if err := tx.Commit(); err != nil {
		t.Fatal("commit revision update:", err)
	}

	mock.ExpectBegin()
	tx, err = database.Begin()
	if err != nil {
		t.Fatal("begin stale transaction:", err)
	}
	mock.ExpectExec(regexp.QuoteMeta(query)).WithArgs(int64(7)).WillReturnResult(sqlmock.NewResult(0, 0))
	if err := BumpConfigRevisionAtTx(tx, dialect, 7); err != ErrConfigRevisionConflict {
		t.Fatalf("stale revision error = %v, want ErrConfigRevisionConflict", err)
	}
	mock.ExpectRollback()
	if err := tx.Rollback(); err != nil {
		t.Fatal("rollback stale transaction:", err)
	}
	if err := BumpConfigRevisionAtTx(nil, dialect, 7); err == nil {
		t.Fatal("nil transaction was accepted")
	}
	if err := BumpConfigRevisionAtTx(tx, dialect, 0); err == nil {
		t.Fatal("zero expected revision was accepted")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("SQL expectations:", err)
	}
}

func TestFenceConfigRevisionTxLocksAndChecksSelectedRevision(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("create SQL mock:", err)
	}
	defer database.Close()
	dialect := db.Resolve("postgres")
	lockQuery := `UPDATE _kora_config_revision SET revision = revision WHERE id = 1`
	readQuery := `SELECT revision FROM _kora_config_revision WHERE id = 1`

	mock.ExpectBegin()
	tx, err := database.Begin()
	if err != nil {
		t.Fatal("begin matching revision transaction:", err)
	}
	mock.ExpectExec(regexp.QuoteMeta(lockQuery)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta(readQuery)).WillReturnRows(sqlmock.NewRows([]string{"revision"}).AddRow(int64(17)))
	if err := FenceConfigRevisionTx(context.Background(), tx, dialect, 17); err != nil {
		t.Fatalf("fence matching config revision: %v", err)
	}
	mock.ExpectCommit()
	if err := tx.Commit(); err != nil {
		t.Fatal("commit matching revision transaction:", err)
	}

	mock.ExpectBegin()
	tx, err = database.Begin()
	if err != nil {
		t.Fatal("begin stale revision transaction:", err)
	}
	mock.ExpectExec(regexp.QuoteMeta(lockQuery)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta(readQuery)).WillReturnRows(sqlmock.NewRows([]string{"revision"}).AddRow(int64(18)))
	if err := FenceConfigRevisionTx(context.Background(), tx, dialect, 17); err != ErrConfigRevisionConflict {
		t.Fatalf("stale config revision error = %v, want ErrConfigRevisionConflict", err)
	}
	mock.ExpectRollback()
	if err := tx.Rollback(); err != nil {
		t.Fatal("rollback stale revision transaction:", err)
	}
	if err := FenceConfigRevisionTx(context.Background(), nil, dialect, 17); err == nil {
		t.Fatal("nil transaction was accepted")
	}
	if err := FenceConfigRevisionTx(context.Background(), tx, dialect, 0); err == nil {
		t.Fatal("zero expected revision was accepted")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("SQL expectations:", err)
	}
}
