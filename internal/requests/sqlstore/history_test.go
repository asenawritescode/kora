package sqlstore

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/requests"
)

func TestHistoryUsesParticipantVisibilityAndStableKeyset(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, err := New(database, db.Resolve("mysql"))
	if err != nil {
		t.Fatal(err)
	}
	scope := requests.VisibilityScope{Tenant: "tenant-1", Actor: "requester-1"}
	id := contract.RequestID("01K7REQUEST00000000000000000")
	mock.ExpectQuery("SELECT EXISTS \\(SELECT 1 FROM _kora_request r").WithArgs("tenant-1", string(id), "requester-1", "user", "requester-1").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	at := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	previous, _ := json.Marshal(contract.Assignee{Kind: contract.AssigneeUser, ID: "old-user"})
	next, _ := json.Marshal(contract.Assignee{Kind: contract.AssigneeUser, ID: "new-user"})
	mock.ExpectQuery("SELECT h.sequence, h.kind,").WithArgs("tenant-1", string(id), int64(0), "requester-1", "user", "requester-1", int64(2)).
		WillReturnRows(sqlmock.NewRows([]string{"sequence", "kind", "actor_id", "assignment_id", "response", "comment", "previous_assignee_json", "new_assignee_json", "created_at"}).
			AddRow(int64(1), "created", "requester-1", nil, nil, "submitted", nil, nil, at).
			AddRow(int64(2), "reassigned", "manager-1", "01K7ASSIGNMENT000000000000000", nil, "delegated", string(previous), string(next), at))
	page, err := store.History(context.Background(), scope, id, contract.PageRequest{Limit: 1})
	if err != nil {
		t.Fatal("History:", err)
	}
	if len(page.Items) != 1 || page.Items[0].Kind != contract.HistoryCreated || page.NextCursor != "1" {
		t.Fatalf("history page = %#v", page)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestHistoryHidesRequestsOutsideVisibilityScope(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, err := New(database, db.Resolve("mysql"))
	if err != nil {
		t.Fatal(err)
	}
	id := contract.RequestID("01K7REQUEST00000000000000000")
	mock.ExpectQuery("SELECT EXISTS \\(SELECT 1 FROM _kora_request r").WithArgs("tenant-1", string(id), "unrelated-user", "user", "unrelated-user").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	_, err = store.History(context.Background(), requests.VisibilityScope{Tenant: "tenant-1", Actor: "unrelated-user"}, id, contract.PageRequest{Limit: 10})
	var contractErr *contract.Error
	if !errors.As(err, &contractErr) || contractErr.Type != contract.CodeNotFound {
		t.Fatalf("unauthorized history error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
