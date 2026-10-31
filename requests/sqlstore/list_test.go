package sqlstore

import (
	"context"
	"database/sql/driver"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/requests"
)

func TestListVisibleAppliesEachInboxViewAndBoundedLimit(t *testing.T) {
	cases := []struct {
		view     contract.InboxView
		viewArgs []any
	}{
		{contract.InboxAction, []any{"pending", "user", "viewer-1", "pending"}},
		{contract.InboxSent, []any{"viewer-1", "pending", "user", "viewer-1", "pending"}},
		{contract.InboxWaiting, []any{"pending", "user", "viewer-1", "blocked", "responded"}},
		{contract.InboxCompleted, []any{"resolved", "cancelled", "expired", "stale"}},
	}
	for _, tc := range cases {
		t.Run(string(tc.view), func(t *testing.T) {
			database, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			store, err := New(database, db.Resolve("mysql"))
			if err != nil {
				t.Fatal(err)
			}
			scope := requests.VisibilityScope{Tenant: "tenant-1", Actor: "viewer-1"}
			args := []any{"tenant-1", "viewer-1", "user", "viewer-1"}
			args = append(args, tc.viewArgs...)
			args = append(args, int64(21))
			mock.ExpectQuery("SELECT r.id FROM _kora_request").WithArgs(asDriverValues(args)...).WillReturnRows(sqlmock.NewRows([]string{"id"}))
			page, err := store.ListVisible(context.Background(), scope, contract.RequestFilter{Page: contract.PageRequest{Limit: 20}, View: tc.view})
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Items) != 0 || page.NextCursor != "" {
				t.Fatalf("empty list page = %#v", page)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestListVisibleBindsCursorToActorAndFilters(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, err := New(database, db.Resolve("mysql"))
	if err != nil {
		t.Fatal(err)
	}
	cursorData, err := json.Marshal(requestListCursor{Tenant: "tenant-1", Actor: "someone-else", View: contract.InboxSent, CreatedAt: "2026-10-14T10:00:00Z", ID: "01K7REQUEST00000000000000000"})
	if err != nil {
		t.Fatal(err)
	}
	cursor := contract.PageCursor(base64.RawURLEncoding.EncodeToString(cursorData))
	_, err = store.ListVisible(context.Background(), requests.VisibilityScope{Tenant: "tenant-1", Actor: "viewer-1"}, contract.RequestFilter{Page: contract.PageRequest{Limit: 10, Cursor: cursor}, View: contract.InboxSent})
	var contractErr *contract.Error
	if !errors.As(err, &contractErr) || contractErr.Type != contract.CodeValidationFailed {
		t.Fatalf("cross-actor cursor error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestListVisibleOverdueAndKindFiltersUseDatabaseClock(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, err := New(database, db.Resolve("mysql"))
	if err != nil {
		t.Fatal(err)
	}
	args := []any{"tenant-1", "viewer-1", "user", "viewer-1", "viewer-1", "pending", "user", "viewer-1", "pending", "approval", "pending", int64(6)}
	mock.ExpectQuery("SELECT r.id FROM _kora_request").WithArgs(asDriverValues(args)...).WillReturnRows(sqlmock.NewRows([]string{"id"}))
	_, err = store.ListVisible(context.Background(), requests.VisibilityScope{Tenant: "tenant-1", Actor: "viewer-1"}, contract.RequestFilter{
		Page: contract.PageRequest{Limit: 5}, View: contract.InboxSent, Kind: kindPtr(contract.RequestApproval), OverdueOnly: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMatchesInboxViewUsesCurrentAssignmentAndTerminalState(t *testing.T) {
	actor := contract.PrincipalID("reviewer-1")
	base := requests.RequestRecord{Status: contract.RequestPending, Assignments: []requests.AssignmentRecord{{Principal: actor, Status: contract.AssignmentPending}}}
	if !matchesInboxView(base, actor, contract.InboxAction) {
		t.Fatal("pending assignment missing from action view")
	}
	base.Assignments[0].Status = contract.AssignmentBlocked
	if !matchesInboxView(base, actor, contract.InboxWaiting) {
		t.Fatal("blocked assignment missing from waiting view")
	}
	base.Status = contract.RequestResolved
	if !matchesInboxView(base, actor, contract.InboxCompleted) {
		t.Fatal("terminal request missing from completed view")
	}
}

func kindPtr(kind contract.RequestKind) *contract.RequestKind { return &kind }

func asDriverValues(values []any) []driver.Value {
	result := make([]driver.Value, len(values))
	for i, value := range values {
		result[i] = value
	}
	return result
}

func TestListVisibleReturnsRecordAndScopedNextCursor(t *testing.T) {
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
	filter := contract.RequestFilter{Page: contract.PageRequest{Limit: 1}, View: contract.InboxSent}
	mock.ExpectQuery("SELECT r.id FROM _kora_request").WithArgs(
		"tenant-1", "requester-1", "user", "requester-1", "requester-1", "pending", "user", "requester-1", "pending", int64(2),
	).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("01K7REQUEST00000000000000000").AddRow("01K7REQUEST00000000000000001"))
	now := time.Date(2026, 10, 14, 11, 0, 0, 0, time.UTC)
	expectListRequestGet(mock, "01K7REQUEST00000000000000000", scope.Tenant, scope.Actor, now)
	page, err := store.ListVisible(context.Background(), scope, filter)
	if err != nil {
		t.Fatal("ListVisible:", err)
	}
	if len(page.Items) != 1 || page.Items[0].ID != "01K7REQUEST00000000000000000" || page.NextCursor == "" {
		t.Fatalf("list page = %#v", page)
	}
	var cursor requestListCursor
	decoded, err := base64.RawURLEncoding.DecodeString(string(page.NextCursor))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(decoded, &cursor); err != nil {
		t.Fatal(err)
	}
	if cursor.Tenant != string(scope.Tenant) || cursor.Actor != string(scope.Actor) || cursor.View != filter.View || cursor.ID != page.Items[0].ID || cursor.CreatedAt != now.Add(-time.Minute).Format(time.RFC3339Nano) {
		t.Fatalf("cursor = %#v", cursor)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func expectListRequestGet(mock sqlmock.Sqlmock, id contract.RequestID, tenant contract.TenantID, requester contract.PrincipalID, now time.Time) {
	mock.ExpectQuery("SELECT\\s+id, tenant_id, revision, kind, assignment_mode,").WithArgs(string(tenant), string(id)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "tenant_id", "revision", "kind", "assignment_mode", "status", "requester_id", "supersedes_request_id", "subject_kind", "subject_id", "subject_revision", "subject_digest", "title", "message", "due_at", "outcome", "resolved_at", "continuation_operation_id", "continuation_state", "policy_id", "policy_revision", "information_schema_id", "information_schema_version", "source_instance_id", "source_step_id", "created_at", "updated_at"}).
			AddRow(string(id), string(tenant), int64(1), "review", "any", "pending", string(requester), nil, nil, nil, nil, "", "Review", "Please review", nil, nil, nil, "", "", "", "", "", int64(0), "", "", now.Add(-time.Minute), now))
	mock.ExpectQuery("SELECT id, request_id, stage, assignee_kind,").WithArgs(string(tenant), string(id)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "request_id", "stage", "assignee_kind", "assignee_id", "original_assignee_kind", "original_assignee_id", "status", "allowed_responses", "membership_revision"}))
}
