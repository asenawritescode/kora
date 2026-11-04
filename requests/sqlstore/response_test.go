package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/requests"
)

func TestResponseReturnsAuthorizedDecisionAndInformation(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, err := New(database, db.Resolve("mysql"))
	if err != nil {
		t.Fatal(err)
	}
	scope := requests.VisibilityScope{Tenant: "tenant-1", Actor: "alice"}
	requestID := contract.RequestID("01K7REQUEST00000000000000000")
	decisionID := contract.DecisionID("01K7DECISION000000000000000000")
	at := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	info, err := json.Marshal(contract.InformationResponse{SchemaID: "review-answer", SchemaVersion: 2, Values: json.RawMessage(`{"note":"ok"}`)})
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery("SELECT d.id, d.tenant_id, d.request_id,").WithArgs("tenant-1", string(requestID), string(decisionID), "alice", "user", "alice").
		WillReturnRows(sqlmock.NewRows([]string{"id", "tenant_id", "request_id", "assignment_id", "actor_id", "on_behalf_of_id", "response", "comment", "information_json", "created_at"}).
			AddRow(string(decisionID), "tenant-1", string(requestID), "01K7ASSIGNMENT000000000000000", "bob", nil, "provide_information", "Here is the detail", string(info), at))
	got, err := store.Response(context.Background(), scope, requestID, decisionID)
	if err != nil {
		t.Fatal("Response:", err)
	}
	if got.Actor != "bob" || got.Response != contract.ResponseProvide || got.Information == nil || got.Information.SchemaID != "review-answer" || got.At != at {
		t.Fatalf("response = %#v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestResponseHidesDecisionFromUnrelatedPrincipal(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, err := New(database, db.Resolve("mysql"))
	if err != nil {
		t.Fatal(err)
	}
	requestID := contract.RequestID("01K7REQUEST00000000000000000")
	decisionID := contract.DecisionID("01K7DECISION000000000000000000")
	mock.ExpectQuery("SELECT d.id, d.tenant_id, d.request_id,").WithArgs("tenant-1", string(requestID), string(decisionID), "mallory", "user", "mallory").WillReturnError(sql.ErrNoRows)
	_, err = store.Response(context.Background(), requests.VisibilityScope{Tenant: "tenant-1", Actor: "mallory"}, requestID, decisionID)
	var contractErr *contract.Error
	if !errors.As(err, &contractErr) || contractErr.Type != contract.CodeNotFound {
		t.Fatalf("unrelated response error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
