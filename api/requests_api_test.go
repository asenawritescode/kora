package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/policy"
	"github.com/asenawritescode/kora/requests"
	"github.com/gin-gonic/gin"
)

func requestTestRouter(execution any, runtime SiteRuntimeService) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("site_name", "acme")
		c.Set("site_id", "tenant-stable-id")
		c.Set("site_runtime_services", runtime)
		if execution != nil {
			c.Set("policy_execution", execution)
		}
		c.Next()
	})
	RegisterRequestRoutes(router.Group("/api/v2"), nil)
	return router
}

type approvalRouteStore struct {
	requests.Store
	record requests.RequestRecord
}

func (s approvalRouteStore) Get(context.Context, contract.TenantID, contract.RequestID) (requests.RequestRecord, error) {
	return s.record, nil
}

type approvalRouteAuthorizer struct{}

func (approvalRouteAuthorizer) Submission(context.Context, policy.Execution, contract.SubmitRequest) (requests.SubmissionAuthorization, error) {
	return requests.SubmissionAuthorization{}, nil
}
func (approvalRouteAuthorizer) Response(context.Context, policy.Execution, requests.RequestRecord, contract.RespondRequest) (requests.ResponseAuthorization, error) {
	return requests.ResponseAuthorization{}, nil
}
func (approvalRouteAuthorizer) Management(context.Context, policy.Execution, requests.RequestRecord, string, *contract.ReassignRequest) (requests.ManagementAuthorization, error) {
	return requests.ManagementAuthorization{}, nil
}
func (approvalRouteAuthorizer) Visibility(_ context.Context, execution policy.Execution) (requests.VisibilityScope, error) {
	return requests.VisibilityScope{Tenant: execution.Tenant(), Actor: contract.PrincipalID(execution.Actor().PrincipalID)}, nil
}

type approvalRouteApplier struct{ called bool }

func (a *approvalRouteApplier) ApproveAndApply(_ context.Context, _ policy.Execution, id contract.RequestID, command contract.RespondRequest) (contract.ResponseReceipt, error) {
	a.called = true
	if command.Response != contract.ResponseReject {
		return contract.ResponseReceipt{}, contract.NewError(contract.CodeValidationFailed, "expected rejection")
	}
	outcome := contract.OutcomeRejected
	return contract.ResponseReceipt{RequestID: id, Revision: "3", Status: contract.RequestResolved, Outcome: &outcome}, nil
}

type approvalRouteClock struct{}

func (approvalRouteClock) Now() time.Time { return time.Now().UTC() }

type approvalRouteIDs struct{}

func (approvalRouteIDs) NewID() string { return contract.NewID() }

func TestRequestAPIUsesCoordinatorForGovernedRejection(t *testing.T) {
	execution, err := policy.NewVerifiedExecution("tenant-stable-id", contract.ActorContext{
		PrincipalID: "reviewer", PrincipalType: contract.PrincipalHuman, SubjectUserID: "reviewer", Site: "acme",
		AuthenticatedAt: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	requestID := contract.RequestID(contract.NewID())
	record := requests.RequestRecord{Tenant: execution.Tenant(), ID: requestID, Revision: "2", Kind: contract.RequestApproval,
		Status: contract.RequestPending, Requester: "requester", Subject: &contract.SubjectRef{Kind: "Purchase Request", ID: "PR-1", Revision: "1"},
		PolicyID: "purchase-request-approval-v1", Title: "Review purchase", Assignments: []requests.AssignmentRecord{
			{ID: "assignment-reviewer", Principal: "reviewer", Status: contract.AssignmentPending, AllowedResponses: []contract.ResponseKind{contract.ResponseReject}},
		}}
	service, err := requests.NewService(approvalRouteStore{record: record}, approvalRouteAuthorizer{}, requests.ParticipantProjector{}, requests.NoResponseSchemas{}, approvalRouteClock{}, approvalRouteIDs{})
	if err != nil {
		t.Fatal(err)
	}
	applier := &approvalRouteApplier{}
	router := requestTestRouter(execution, SiteRuntimeService{Requests: service, ApprovalApplier: applier})
	command := contract.RespondRequest{Meta: contract.CommandMeta{IdempotencyKey: "reject-key", ExpectedRevision: "2"},
		AssignmentID: "assignment-reviewer", Response: contract.ResponseReject}
	body, err := json.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v2/requests/"+string(requestID)+"/responses", bytes.NewReader(body)))
	if recorder.Code != http.StatusOK || !applier.called {
		t.Fatalf("rejection status=%d coordinator called=%v body=%s", recorder.Code, applier.called, recorder.Body.String())
	}
}

func TestRequestRoutesRequireVerifiedExecution(t *testing.T) {
	router := requestTestRouter(nil, SiteRuntimeService{})
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v2/requests", nil))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, body = %s; want 401", recorder.Code, recorder.Body.String())
	}
}

func TestRequestRoutesRejectExecutionForDifferentTenantRoute(t *testing.T) {
	execution, err := policy.NewVerifiedExecution("other-tenant", contract.ActorContext{
		PrincipalID: "alice", PrincipalType: contract.PrincipalHuman, SubjectUserID: "alice", Site: "other-site",
		AuthenticatedAt: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	router := requestTestRouter(execution, SiteRuntimeService{})
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v2/requests", nil))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, body = %s; want 401", recorder.Code, recorder.Body.String())
	}
}

func TestRequestRoutesFailClosedWhenTenantServiceIsNotAssembled(t *testing.T) {
	execution, err := policy.NewVerifiedExecution("tenant-stable-id", contract.ActorContext{
		PrincipalID: "alice", PrincipalType: contract.PrincipalHuman, SubjectUserID: "alice", Site: "acme",
		AuthenticatedAt: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	router := requestTestRouter(execution, SiteRuntimeService{})
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v2/requests", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s; want 503", recorder.Code, recorder.Body.String())
	}
}
