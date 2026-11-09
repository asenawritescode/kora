package sqlstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/policy"
	"github.com/asenawritescode/kora/internal/requests"
)

func directExecution(t *testing.T, actor, site string) policy.Execution {
	t.Helper()
	value, err := policy.NewVerifiedExecution("tenant-stable-id", contract.ActorContext{
		PrincipalID: actor, PrincipalType: contract.PrincipalHuman, SubjectUserID: actor, Site: site, AuthenticatedAt: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func TestDirectTaskAuthorizerExpandsActiveUsersAndSnapshotsRevisions(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	authorizer, err := NewDirectTaskAuthorizer(database, db.Resolve("mysql"))
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery("SELECT policy_revision, membership_revision FROM _kora_auth_revision WHERE id = 1").WillReturnRows(sqlmock.NewRows([]string{"policy_revision", "membership_revision"}).AddRow(3, 7))
	mock.ExpectQuery("SELECT enabled FROM _kora_user WHERE site = \\? AND name = \\?").WithArgs("acme", "requester").WillReturnRows(sqlmock.NewRows([]string{"enabled"}).AddRow(true))
	mock.ExpectQuery("SELECT name FROM _kora_user WHERE site = \\? AND enabled = \\? AND name IN \\(\\?,\\?\\)").WithArgs("acme", true, "user-a", "user-b").WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("user-a").AddRow("user-b"))
	result, err := authorizer.Submission(context.Background(), directExecution(t, "requester", "acme"), contract.SubmitRequest{
		Kind: contract.RequestTask, Title: "Review", Assignment: &contract.AssignmentSpec{Mode: contract.AssignmentSequential, Assignees: []contract.Assignee{{Kind: contract.AssigneeUser, ID: "user-a"}, {Kind: contract.AssigneeUser, ID: "user-b"}}},
	})
	if err != nil {
		t.Fatalf("Submission() error = %v", err)
	}
	if result.PolicyRevision != "3" || result.MembershipRevision != "7" || result.AssignmentMode != contract.AssignmentSequential {
		t.Fatalf("authorization snapshot = %+v", result)
	}
	if len(result.Assignments) != 2 || result.Assignments[0].Status != contract.AssignmentPending || result.Assignments[1].Status != contract.AssignmentBlocked || result.Assignments[1].Stage != 2 {
		t.Fatalf("sequential assignment snapshot = %+v", result.Assignments)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDirectTaskAuthorizerRejectsGovernedAndSubjectBoundRequestsBeforeDatabase(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	authorizer, err := NewDirectTaskAuthorizer(database, db.Resolve("mysql"))
	if err != nil {
		t.Fatal(err)
	}
	exec := directExecution(t, "requester", "acme")
	for _, command := range []contract.SubmitRequest{
		{Kind: contract.RequestApproval, PolicyID: "approval-v1", Subject: &contract.SubjectRef{Kind: "invoice", ID: "INV-1", Revision: "1"}},
		{Kind: contract.RequestTask, PolicyID: "client-controlled-policy", Assignment: &contract.AssignmentSpec{Mode: contract.AssignmentAny, Assignees: []contract.Assignee{{Kind: contract.AssigneeUser, ID: "user-a"}}}},
		{Kind: contract.RequestTask, Subject: &contract.SubjectRef{Kind: "invoice", ID: "INV-1", Revision: "1"}, Assignment: &contract.AssignmentSpec{Mode: contract.AssignmentAny, Assignees: []contract.Assignee{{Kind: contract.AssigneeUser, ID: "user-a"}}}},
	} {
		if _, err := authorizer.Submission(context.Background(), exec, command); err == nil {
			t.Fatal("direct task authorizer accepted a governed or subject-bound request")
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unsupported request shape reached persistence: %v", err)
	}
}

func TestDirectTaskAuthorizerRechecksAssigneeMembershipAndResponseEligibility(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	authorizer, err := NewDirectTaskAuthorizer(database, db.Resolve("mysql"))
	if err != nil {
		t.Fatal(err)
	}
	exec := directExecution(t, "assignee", "acme")
	record := requestsTestRecord()
	command := contract.RespondRequest{AssignmentID: "assignment-1", Response: contract.ResponseComplete}
	mock.ExpectQuery("SELECT policy_revision, membership_revision FROM _kora_auth_revision WHERE id = 1").WillReturnRows(sqlmock.NewRows([]string{"policy_revision", "membership_revision"}).AddRow(4, 9))
	mock.ExpectQuery("SELECT enabled FROM _kora_user WHERE site = \\? AND name = \\?").WithArgs("acme", "assignee").WillReturnRows(sqlmock.NewRows([]string{"enabled"}).AddRow(true))
	result, err := authorizer.Response(context.Background(), exec, record, command)
	if err != nil || result.PolicyRevision != "4" || result.MembershipRevision != "9" {
		t.Fatalf("Response() result=%+v error=%v", result, err)
	}
	command.Response = contract.ResponseReject
	if _, err := authorizer.Response(context.Background(), exec, record, command); err == nil {
		t.Fatal("Response() accepted a response outside the assignment-specific allowed set")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDirectTaskAuthorizerRejectsRevokedTenantUser(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	authorizer, err := NewDirectTaskAuthorizer(database, db.Resolve("mysql"))
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery("SELECT policy_revision, membership_revision FROM _kora_auth_revision WHERE id = 1").WillReturnRows(sqlmock.NewRows([]string{"policy_revision", "membership_revision"}).AddRow(4, 9))
	mock.ExpectQuery("SELECT enabled FROM _kora_user WHERE site = \\? AND name = \\?").WithArgs("acme", "assignee").WillReturnRows(sqlmock.NewRows([]string{"enabled"}).AddRow(false))
	_, err = authorizer.Response(context.Background(), directExecution(t, "assignee", "acme"), requestsTestRecord(), contract.RespondRequest{AssignmentID: "assignment-1", Response: contract.ResponseComplete})
	var apiError *contract.Error
	if !errors.As(err, &apiError) || apiError.Type != contract.CodePermissionDenied {
		t.Fatalf("revoked response error = %v, want permission denied", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func requestsTestRecord() requests.RequestRecord {
	return requests.RequestRecord{Tenant: "tenant-stable-id", ID: "01K7REQUEST00000000000000000", Kind: contract.RequestTask, Status: contract.RequestPending, Requester: "requester", Revision: "1",
		Assignments: []requests.AssignmentRecord{{ID: "assignment-1", RequestID: "01K7REQUEST00000000000000000", Principal: "assignee", Status: contract.AssignmentPending, AllowedResponses: []contract.ResponseKind{contract.ResponseComplete}}}}
}
