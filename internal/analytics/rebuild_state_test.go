package analytics

import (
	"context"
	"database/sql"
	"reflect"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/internal/db"
)

func TestCurrentSourceSequenceFallsBackToLegacyMaximum(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mock.ExpectBegin()
	tx, err := database.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery(`SELECT last_sequence FROM _kora_tenant_event_sequence WHERE id = 1`).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(`SELECT COALESCE\(MAX\(last_sequence\), 0\) FROM _kora_event_sequence`).
		WillReturnRows(sqlmock.NewRows([]string{"last_sequence"}).AddRow(14))
	sequence, err := currentSourceSequenceTx(context.Background(), tx, db.Resolve("mysql"))
	if err != nil || sequence != 14 {
		t.Fatalf("currentSourceSequenceTx() = %d, %v; want 14, nil", sequence, err)
	}
	mock.ExpectRollback()
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestContributionFilteringUsesTenantSequenceAndRebuildScope(t *testing.T) {
	contribution := eventContribution{sequence: "8", deltas: map[deltaKey]float64{
		{Doctype: "Task", Metric: "count", Date: "2026-01-15"}:    1,
		{Doctype: "Task", Metric: "count", Date: "2025-12-31"}:    -1,
		{Doctype: "Invoice", Metric: "count", Date: "2026-01-15"}: 1,
	}}
	checkpoints := []rebuildCheckpoint{{doctype: "Task", from: "2026-01-01", sequence: 10}}
	got, err := contributionOutsideRebuiltRange(contribution, contribution.sequence, checkpoints)
	if err != nil {
		t.Fatal(err)
	}
	want := map[deltaKey]float64{
		{Doctype: "Task", Metric: "count", Date: "2025-12-31"}:    -1,
		{Doctype: "Invoice", Metric: "count", Date: "2026-01-15"}: 1,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("accepted deltas = %#v, want %#v", got, want)
	}

	contribution.sequence = "11"
	got, err = contributionOutsideRebuiltRange(contribution, contribution.sequence, checkpoints)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, contribution.deltas) {
		t.Fatalf("post-cutoff deltas = %#v, want all contributions %#v", got, contribution.deltas)
	}
}

func TestEventAtOrBeforeSequenceRejectsNoncanonicalValues(t *testing.T) {
	for _, sequence := range []string{"", "00", "-1", "1.0", "18446744073709551616"} {
		if sequence == "" {
			if got, err := eventAtOrBeforeSequence(sequence, 1); err != nil || got {
				t.Errorf("empty event sequence = (%v, %v), want (false, nil)", got, err)
			}
			continue
		}
		if _, err := eventAtOrBeforeSequence(sequence, 1); err == nil {
			t.Errorf("event sequence %q was accepted", sequence)
		}
	}
}
