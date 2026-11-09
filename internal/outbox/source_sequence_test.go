package outbox

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/internal/db"
)

func TestNextSourceSequenceRejectsSignedBigIntExhaustion(t *testing.T) {
	for _, tc := range []struct {
		name      string
		current   int64
		legacyMax int64
	}{
		{name: "allocator at maximum", current: maxSourceSequence},
		{name: "legacy allocator at maximum", current: 1, legacyMax: maxSourceSequence},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal("create SQL mock:", err)
			}
			defer database.Close()

			mock.ExpectBegin()
			tx, err := database.BeginTx(context.Background(), nil)
			if err != nil {
				t.Fatal("begin fixture transaction:", err)
			}
			mock.ExpectExec(regexp.QuoteMeta("INSERT INTO _kora_tenant_event_sequence (id, last_sequence) VALUES (1, 0) ON DUPLICATE KEY UPDATE id = VALUES(id)")).WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectQuery(regexp.QuoteMeta("SELECT last_sequence FROM _kora_tenant_event_sequence WHERE id = 1 FOR UPDATE")).WillReturnRows(sqlmock.NewRows([]string{"last_sequence"}).AddRow(tc.current))
			mock.ExpectQuery(regexp.QuoteMeta("SELECT COALESCE(MAX(last_sequence), 0) FROM _kora_event_sequence")).WillReturnRows(sqlmock.NewRows([]string{"legacy_max"}).AddRow(tc.legacyMax))
			mock.ExpectRollback()

			sequence, err := nextSourceSequence(context.Background(), tx, db.Resolve("mysql"))
			if !errors.Is(err, ErrSourceSequenceExhausted) {
				t.Fatalf("nextSourceSequence() = (%d, %v), want signed BIGINT exhaustion", sequence, err)
			}
			if err := tx.Rollback(); err != nil {
				t.Fatal("rollback exhausted sequence allocation:", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal("SQL expectations:", err)
			}
		})
	}
}

func TestNextSourceSequenceSeedsLegacyValueWithoutOverflow(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("create SQL mock:", err)
	}
	defer database.Close()

	mock.ExpectBegin()
	tx, err := database.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal("begin fixture transaction:", err)
	}
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO _kora_tenant_event_sequence (id, last_sequence) VALUES (1, 0) ON DUPLICATE KEY UPDATE id = VALUES(id)")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT last_sequence FROM _kora_tenant_event_sequence WHERE id = 1 FOR UPDATE")).WillReturnRows(sqlmock.NewRows([]string{"last_sequence"}).AddRow(int64(3)))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COALESCE(MAX(last_sequence), 0) FROM _kora_event_sequence")).WillReturnRows(sqlmock.NewRows([]string{"legacy_max"}).AddRow(int64(14)))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE _kora_tenant_event_sequence SET last_sequence = ? WHERE id = 1")).WithArgs(int64(15)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	sequence, err := nextSourceSequence(context.Background(), tx, db.Resolve("mysql"))
	if err != nil || sequence != 15 {
		t.Fatalf("nextSourceSequence() = (%d, %v), want (15, nil)", sequence, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal("commit source sequence:", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("SQL expectations:", err)
	}
}
