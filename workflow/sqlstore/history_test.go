package sqlstore

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/db"
)

func TestListHistoryIsTenantScopedAndReturnsChronologicalRows(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, err := NewInstanceStore(database, db.Resolve("postgres"))
	if err != nil {
		t.Fatal(err)
	}
	created := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT sequence,step_id,kind,status,detail,created_at FROM _kora_workflow_history WHERE tenant_id=$1 AND instance_id=$2 ORDER BY sequence DESC LIMIT $3`)).
		WithArgs("tenant-a", "01K7INSTANCE00000000000000000", uint16(20)).
		WillReturnRows(sqlmock.NewRows([]string{"sequence", "step_id", "kind", "status", "detail", "created_at"}).
			AddRow(2, "send-email", "step_committed", "failed", "mail transport failed", created.Add(time.Minute)).
			AddRow(1, "start", "started", "ready", "", created))
	entries, err := store.ListHistory(context.Background(), "tenant-a", "01K7INSTANCE00000000000000000", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Sequence != 1 || entries[1].Status != "failed" || entries[1].Detail != "mail transport failed" {
		t.Fatalf("unexpected workflow history: %#v", entries)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
