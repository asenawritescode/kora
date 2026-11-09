//go:build integration

package process

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/dbaccess"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/kernel"
	"github.com/asenawritescode/kora/internal/outbox"
	"github.com/asenawritescode/kora/internal/policy"
	"github.com/asenawritescode/kora/internal/requests"
	requestsql "github.com/asenawritescode/kora/internal/requests/sqlstore"
	"github.com/asenawritescode/kora/internal/schema"
	"github.com/asenawritescode/kora/internal/site"
	"github.com/asenawritescode/kora/internal/workflow"
	workflowsql "github.com/asenawritescode/kora/internal/workflow/sqlstore"
	_ "github.com/go-sql-driver/mysql"
)

const integrationApprovalPolicy = "purchase-approval-v1"

type mysqlApprovalAuthorizer struct {
	tenant    contract.TenantID
	requester contract.PrincipalID
	approver  contract.PrincipalID
	approver2 contract.PrincipalID
	mode      contract.AssignmentMode
	subject   contract.SubjectRef
	digest    string
}

func (a mysqlApprovalAuthorizer) Submission(_ context.Context, exec policy.Execution, cmd contract.SubmitRequest) (requests.SubmissionAuthorization, error) {
	if exec.Tenant() != a.tenant || cmd.PolicyID != integrationApprovalPolicy || cmd.Subject == nil || *cmd.Subject != a.subject {
		return requests.SubmissionAuthorization{}, contract.NewError(contract.CodePermissionDenied, "approval policy mismatch")
	}
	mode := a.mode
	if mode == "" {
		mode = contract.AssignmentAny
	}
	assignments := []requests.AssignmentRecord{{Principal: a.approver, OriginalTarget: contract.Assignee{Kind: contract.AssigneeUser, ID: string(a.approver)}, Stage: 1, Status: contract.AssignmentPending,
		AllowedResponses: []contract.ResponseKind{contract.ResponseApprove, contract.ResponseReject, contract.ResponseReturn}, MembershipRevision: "1"}}
	if a.approver2 != "" {
		assignments = append(assignments, requests.AssignmentRecord{Principal: a.approver2, OriginalTarget: contract.Assignee{Kind: contract.AssigneeUser, ID: string(a.approver2)}, Stage: 2, Status: contract.AssignmentPending,
			AllowedResponses: []contract.ResponseKind{contract.ResponseApprove, contract.ResponseReject, contract.ResponseReturn}, MembershipRevision: "1"})
	}
	return requests.SubmissionAuthorization{PolicyID: integrationApprovalPolicy, PolicyRevision: "1", MembershipRevision: "1",
		ExpectedSubject: &a.subject, SubjectDigest: a.digest, AssignmentMode: mode, Assignments: assignments}, nil
}
func (a mysqlApprovalAuthorizer) Response(_ context.Context, exec policy.Execution, record requests.RequestRecord, _ contract.RespondRequest) (requests.ResponseAuthorization, error) {
	if exec.Tenant() != a.tenant || record.PolicyID != integrationApprovalPolicy {
		return requests.ResponseAuthorization{}, contract.NewError(contract.CodePermissionDenied, "approval policy mismatch")
	}
	return requests.ResponseAuthorization{PolicyRevision: "1", MembershipRevision: "1", ExpectedSubject: &a.subject, SubjectDigest: a.digest}, nil
}
func (mysqlApprovalAuthorizer) Management(context.Context, policy.Execution, requests.RequestRecord, string, *contract.ReassignRequest) (requests.ManagementAuthorization, error) {
	return requests.ManagementAuthorization{}, contract.NewError(contract.CodePermissionDenied, "not used")
}
func (a mysqlApprovalAuthorizer) Visibility(_ context.Context, exec policy.Execution) (requests.VisibilityScope, error) {
	return requests.VisibilityScope{Tenant: a.tenant, Actor: contract.PrincipalID(exec.Actor().PrincipalID)}, nil
}

type mysqlApprovalProjector struct{}

func (mysqlApprovalProjector) Request(_ context.Context, _ policy.Execution, record requests.RequestRecord) (contract.RequestView, error) {
	assignments := make([]contract.AssignmentView, 0, len(record.Assignments))
	for _, assignment := range record.Assignments {
		assignments = append(assignments, contract.AssignmentView{ID: assignment.ID, Stage: assignment.Stage,
			Assignee: contract.Assignee{Kind: contract.AssigneeUser, ID: string(assignment.Principal)}, Status: assignment.Status,
			AllowedResponses: append([]contract.ResponseKind(nil), assignment.AllowedResponses...)})
	}
	return contract.RequestView{ID: record.ID, Revision: record.Revision, Kind: record.Kind, Status: record.Status, Requester: record.Requester, Assignments: assignments}, nil
}
func (mysqlApprovalProjector) History(context.Context, policy.Execution, requests.RequestRecord, contract.RequestHistoryEntry) (contract.RequestHistoryEntry, error) {
	return contract.RequestHistoryEntry{}, nil
}
func (mysqlApprovalProjector) Response(context.Context, policy.Execution, requests.RequestRecord, requests.DecisionRecord) (contract.ResponseDetail, error) {
	return contract.ResponseDetail{}, nil
}

type mysqlApprovalSchemas struct{}

func (mysqlApprovalSchemas) Registered(context.Context, contract.SchemaRef) error { return nil }
func (mysqlApprovalSchemas) Validate(context.Context, contract.SchemaRef, contract.InformationResponse) error {
	return nil
}

type mysqlApprovalClock struct{}

func (mysqlApprovalClock) Now() time.Time { return time.Now().UTC() }

type mysqlApprovalIDs struct{}

func (mysqlApprovalIDs) NewID() string { return contract.NewID() }

func TestApproveAndApplyCommitsDecisionAndDomainTransitionAtomically(t *testing.T) {
	dsn := os.Getenv("KORA_PROCESS_DISPOSABLE_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set KORA_PROCESS_DISPOSABLE_MYSQL_DSN to a disposable MySQL database")
	}
	database, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal("open disposable MySQL:", err)
	}
	database.SetMaxOpenConns(4)
	database.SetMaxIdleConns(2)
	t.Cleanup(func() { _ = database.Close() })
	if err := database.Ping(); err != nil {
		t.Fatal("ping disposable MySQL:", err)
	}
	dialect := db.Resolve("mysql")
	if err = site.BootstrapSystemTables(context.Background(), database, dialect); err != nil {
		t.Fatal("bootstrap Engine tables:", err)
	}
	requestDDL, err := requestsql.SchemaStatements(dialect)
	if err != nil {
		t.Fatal("request schema:", err)
	}
	for _, statement := range requestDDL {
		if _, err := database.Exec(statement); err != nil {
			t.Fatalf("apply request schema: %v\n%s", err, statement)
		}
	}
	var suffix string
	if err := database.QueryRow("SELECT REPLACE(UUID(), '-', '')").Scan(&suffix); err != nil {
		t.Fatal("generate integration suffix:", err)
	}
	siteName := "approve-apply-" + suffix[:12]
	tenant := contract.TenantID("tenant-" + suffix)
	doctypeName := "ApprovalInvoice" + suffix[:12]
	dt := &doctype.DocType{Name: doctypeName, Fields: []doctype.Field{{Fieldname: "title", Fieldtype: "Data", Reqd: true}, {Fieldname: "status", Fieldtype: "Data"}}}
	registry := doctype.NewRegistry()
	registry.LoadFull([]*doctype.DocType{dt}, []*doctype.Role{{Name: "Manager"}}, []*doctype.Permission{{Doctype: doctypeName, Role: "Manager", Read: true, Create: true, Write: true, Submit: true}})
	var databaseName string
	if err := database.QueryRow("SELECT DATABASE()").Scan(&databaseName); err != nil {
		t.Fatal("read disposable database name:", err)
	}
	if err := schema.MigrateSiteFromRegistry(database, databaseName, registry, dialect); err != nil {
		t.Fatal("migrate invoice doctype:", err)
	}
	engine := kernel.New(dialect, outbox.NewSQLWriter(dialect))
	workflowDefinitions, err := workflowsql.NewDefinitionStore(database, dialect)
	if err != nil {
		t.Fatal("workflow definition store:", err)
	}
	if err := workflowDefinitions.Ensure(context.Background()); err != nil {
		t.Fatal("ensure workflow definition schema:", err)
	}
	statusInput, err := json.Marshal(workflow.StatusActionConfig{StateField: "status", InitialState: "Draft", StatusTransition: workflow.StatusTransition{Action: "approve", From: "Draft", To: "Approved", AllowedRoles: []string{"Manager"}, DocStatus: 1}})
	if err != nil {
		t.Fatal(err)
	}
	statusDefinition := contract.WorkflowDefinition{ID: contract.DefinitionID("approval-status-" + suffix[:8]), Version: 1, Entry: "transition",
		Trigger: &contract.RecordTrigger{Kind: "record.updated", Table: doctypeName, WatchedFields: []string{"status"}}, Steps: []contract.StepDefinition{
			{ID: "transition", Kind: contract.StepAction, Action: &contract.ActionSpec{Name: "record.update_status", Version: 1, InputSchema: "kora.record.update_status", Input: statusInput, Next: "finish"}},
			{ID: "finish", Kind: contract.StepEnd, End: &contract.EndSpec{Outcome: "complete"}},
		}}
	migrationActor, err := contract.ParsePrincipalID("system-approval-migration")
	if err != nil {
		t.Fatal("workflow migration actor:", err)
	}
	if err := workflowDefinitions.PublishImportedBatch(context.Background(), tenant, migrationActor, []contract.WorkflowDefinition{statusDefinition}, time.Now().UTC()); err != nil {
		t.Fatal("publish v2 status workflow:", err)
	}
	engine.WorkflowStatusPolicies = workflowDefinitions
	approverExecution, err := policy.NewVerifiedExecution(tenant, contract.ActorContext{PrincipalID: "approver-" + suffix[:8], PrincipalType: contract.PrincipalHuman,
		SubjectUserID: "approver-" + suffix[:8], Site: siteName, Roles: []string{"Manager"}, AuthenticatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal("approver execution:", err)
	}
	createData := []byte(`{"title":"Request approval","status":"Draft"}`)
	createPayload, err := json.Marshal(kernel.RecordCreatePayload{Doctype: doctypeName, Data: createData})
	if err != nil {
		t.Fatal("encode invoice create:", err)
	}
	created, commandErr := engine.Execute(context.Background(), database, registry, kernel.Operation{Command: kernel.CommandRecordCreate, Payload: createPayload,
		Context: kernel.OperationContext{Site: siteName, TenantID: tenant, Execution: approverExecution, Source: kernel.SourceIntegration}})
	if commandErr != nil || created.Status != contract.StatusCompleted {
		t.Fatalf("create invoice: result=%+v error=%v", created, commandErr)
	}
	var invoice kernel.ResultData
	if err := json.Unmarshal(created.Data, &invoice); err != nil {
		t.Fatal("decode invoice:", err)
	}
	requestID := contract.RequestID("")
	requestStore, err := requestsql.New(database, dialect)
	if err != nil {
		t.Fatal("request SQL store:", err)
	}
	digestBytes := sha256.Sum256([]byte("invoice-content-v1"))
	secondApproverID := "approver-second-" + suffix[:8]
	secondApproverExecution, err := policy.NewVerifiedExecution(tenant, contract.ActorContext{PrincipalID: secondApproverID, PrincipalType: contract.PrincipalHuman,
		SubjectUserID: secondApproverID, Site: siteName, Roles: []string{"Manager"}, AuthenticatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal("second approver execution:", err)
	}
	authorizer := mysqlApprovalAuthorizer{tenant: tenant, requester: contract.PrincipalID("requester-" + suffix[:8]), approver: contract.PrincipalID(approverExecution.Actor().PrincipalID),
		approver2: contract.PrincipalID(secondApproverExecution.Actor().PrincipalID), mode: contract.AssignmentAll,
		subject: contract.SubjectRef{Kind: doctypeName, ID: invoice.Name, Revision: "1"}, digest: fmt.Sprintf("%x", digestBytes)}
	requestService, err := requests.NewService(requestStore, authorizer, mysqlApprovalProjector{}, mysqlApprovalSchemas{}, mysqlApprovalClock{}, mysqlApprovalIDs{})
	if err != nil {
		t.Fatal("request service:", err)
	}
	requesterExecution, err := policy.NewVerifiedExecution(tenant, contract.ActorContext{PrincipalID: string(authorizer.requester), PrincipalType: contract.PrincipalHuman,
		SubjectUserID: string(authorizer.requester), Site: siteName, Roles: []string{"Manager"}, AuthenticatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal("requester execution:", err)
	}
	view, err := requestService.Submit(context.Background(), requesterExecution, contract.SubmitRequest{Meta: contract.CommandMeta{IdempotencyKey: contract.IdempotencyKey("submit-approval-" + suffix[:8])},
		Kind: contract.RequestApproval, Subject: &authorizer.subject, Title: "Approve invoice", Message: "Approve this invoice?", PolicyID: integrationApprovalPolicy})
	if err != nil {
		t.Fatal("submit approval:", err)
	}
	requestID = view.ID
	if len(view.Assignments) != 2 {
		t.Fatalf("expected two approval assignments, got %d", len(view.Assignments))
	}
	firstCommand := contract.RespondRequest{Meta: contract.CommandMeta{IdempotencyKey: contract.IdempotencyKey("approve-first-" + suffix[:8]), ExpectedRevision: view.Revision},
		AssignmentID: contract.AssignmentID(view.Assignments[0].ID), Response: contract.ResponseApprove}
	openTransactions := 0
	workAdmission, err := dbaccess.NewAdmission(1, 1, time.Second)
	if err != nil {
		t.Fatal("create bounded work admission:", err)
	}
	openTransaction, err := NewTenantTransactionOpener(tenant, database, workAdmission)
	if err != nil {
		t.Fatal("create tenant transaction opener:", err)
	}
	begin := func(ctx context.Context, gotTenant contract.TenantID) (*sql.DB, *sql.Tx, func(), error) {
		openTransactions++
		return openTransaction(ctx, gotTenant)
	}
	coordinator, err := NewCoordinator(requestService, engine, registry, begin, map[string]WorkflowApproval{integrationApprovalPolicy: {SubjectKind: doctypeName, Action: "approve"}})
	if err != nil {
		t.Fatal("coordinator:", err)
	}
	firstReceipt, err := coordinator.ApproveAndApply(context.Background(), approverExecution, requestID, firstCommand)
	if err != nil {
		t.Fatal("first approval response:", err)
	}
	if firstReceipt.Status != contract.RequestPending || firstReceipt.Outcome != nil {
		t.Fatalf("first of two approvals prematurely resolved request: %+v", firstReceipt)
	}
	var requestStatus, invoiceStatus string
	if err := database.QueryRow("SELECT status FROM `"+dt.RawTableName()+"` WHERE name = ?", invoice.Name).Scan(&invoiceStatus); err != nil {
		t.Fatal("read invoice after first approval:", err)
	}
	if invoiceStatus != "Draft" {
		t.Fatalf("first approval prematurely advanced domain subject: status=%q", invoiceStatus)
	}
	secondCommand := contract.RespondRequest{Meta: contract.CommandMeta{IdempotencyKey: contract.IdempotencyKey("approve-second-" + suffix[:8]), ExpectedRevision: firstReceipt.Revision},
		AssignmentID: contract.AssignmentID(view.Assignments[1].ID), Response: contract.ResponseApprove}
	failedCoordinator, err := NewCoordinator(requestService, engine, registry, begin, map[string]WorkflowApproval{integrationApprovalPolicy: {SubjectKind: doctypeName, Action: "missing-action"}})
	if err != nil {
		t.Fatal("failed coordinator:", err)
	}
	if _, err := failedCoordinator.ApproveAndApply(context.Background(), secondApproverExecution, requestID, secondCommand); err == nil {
		t.Fatal("invalid workflow action unexpectedly applied")
	}
	if err := database.QueryRow("SELECT status FROM _kora_request WHERE tenant_id = ? AND id = ?", string(tenant), string(requestID)).Scan(&requestStatus); err != nil {
		t.Fatal("read request after rolled-back action:", err)
	}
	if err := database.QueryRow("SELECT status FROM `"+dt.RawTableName()+"` WHERE name = ?", invoice.Name).Scan(&invoiceStatus); err != nil {
		t.Fatal("read invoice after rolled-back action:", err)
	}
	var decisionsAfterRollback int
	if err := database.QueryRow("SELECT COUNT(*) FROM _kora_request_decision WHERE tenant_id = ? AND request_id = ?", string(tenant), string(requestID)).Scan(&decisionsAfterRollback); err != nil {
		t.Fatal("count decisions after rollback:", err)
	}
	if requestStatus != string(contract.RequestPending) || invoiceStatus != "Draft" || decisionsAfterRollback != 1 {
		t.Fatalf("failed final action escaped rollback: request=%q invoice=%q decisions=%d", requestStatus, invoiceStatus, decisionsAfterRollback)
	}
	receipt, err := coordinator.ApproveAndApply(context.Background(), secondApproverExecution, requestID, secondCommand)
	if err != nil {
		t.Fatal("approve and apply:", err)
	}
	if receipt.Status != contract.RequestResolved || receipt.Outcome == nil || *receipt.Outcome != contract.OutcomeApproved {
		t.Fatalf("approval receipt=%+v", receipt)
	}
	if err := database.QueryRow("SELECT status FROM _kora_request WHERE tenant_id = ? AND id = ?", string(tenant), string(requestID)).Scan(&requestStatus); err != nil {
		t.Fatal("read committed request:", err)
	}
	if err := database.QueryRow("SELECT status FROM `"+dt.RawTableName()+"` WHERE name = ?", invoice.Name).Scan(&invoiceStatus); err != nil {
		t.Fatal("read committed invoice:", err)
	}
	if requestStatus != string(contract.RequestResolved) || invoiceStatus != "Approved" {
		t.Fatalf("request/domain effect not committed together: request=%q invoice=%q", requestStatus, invoiceStatus)
	}
	transactionCount := openTransactions
	replayed, err := coordinator.ApproveAndApply(context.Background(), secondApproverExecution, requestID, secondCommand)
	if err != nil || replayed.RequestID != receipt.RequestID || replayed.DecisionID != receipt.DecisionID || replayed.Revision != receipt.Revision || replayed.Status != receipt.Status || replayed.Outcome == nil || receipt.Outcome == nil || *replayed.Outcome != *receipt.Outcome {
		t.Fatalf("replay receipt=%+v error=%v, want %+v", replayed, err, receipt)
	}
	if openTransactions != transactionCount {
		t.Fatalf("replay opened another transaction: before=%d after=%d", transactionCount, openTransactions)
	}
	var decisions, completedTransitions int
	if err := database.QueryRow("SELECT COUNT(*) FROM _kora_request_decision WHERE tenant_id = ? AND request_id = ?", string(tenant), string(requestID)).Scan(&decisions); err != nil {
		t.Fatal("count decisions:", err)
	}
	if err := database.QueryRow("SELECT COUNT(*) FROM _kora_operation_audit WHERE site = ? AND command_name = ? AND status = 'completed'", siteName, kernel.CommandWorkflowStatusTransition).Scan(&completedTransitions); err != nil {
		t.Fatal("count workflow transition audits:", err)
	}
	if decisions != 2 || completedTransitions != 1 {
		t.Fatalf("duplicate effects after replay: decisions=%d transitions=%d", decisions, completedTransitions)
	}
}
