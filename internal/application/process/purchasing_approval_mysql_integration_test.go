//go:build integration

package process

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/application/purchasing"
	purchasingsql "github.com/asenawritescode/kora/internal/application/purchasing/sqlstore"
	applicationworkflow "github.com/asenawritescode/kora/internal/application/workflow"
	"github.com/asenawritescode/kora/internal/configstore"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/dbaccess"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/eventstream"
	"github.com/asenawritescode/kora/internal/expression"
	"github.com/asenawritescode/kora/internal/kernel"
	"github.com/asenawritescode/kora/internal/outbox"
	"github.com/asenawritescode/kora/internal/policy"
	"github.com/asenawritescode/kora/internal/requests"
	requestsql "github.com/asenawritescode/kora/internal/requests/sqlstore"
	"github.com/asenawritescode/kora/internal/schema"
	"github.com/asenawritescode/kora/internal/site"
	processworkflow "github.com/asenawritescode/kora/internal/workflow"
	workflowsql "github.com/asenawritescode/kora/internal/workflow/sqlstore"
	"github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
	_ "github.com/tursodatabase/libsql-client-go/libsql"
)

func TestPurchaseApprovalPolicyAndDomainTransitionCommitTogether(t *testing.T) {
	dsn := os.Getenv("KORA_PROCESS_DISPOSABLE_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set KORA_PROCESS_DISPOSABLE_MYSQL_DSN to a disposable MySQL database")
	}
	rootConfig, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal("parse disposable MySQL DSN:", err)
	}
	rootConfig.DBName = ""
	root, err := sql.Open("mysql", rootConfig.FormatDSN())
	if err != nil {
		t.Fatal("open disposable MySQL service:", err)
	}
	if err := root.Ping(); err != nil {
		_ = root.Close()
		t.Fatal("ping disposable MySQL service:", err)
	}
	databaseName := fmt.Sprintf("kora_purchase_process_%d", time.Now().UnixNano()%1_000_000_000)
	if _, err := root.Exec("CREATE DATABASE `" + databaseName + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		_ = root.Close()
		t.Fatal("create isolated disposable database:", err)
	}
	t.Cleanup(func() { _, _ = root.Exec("DROP DATABASE IF EXISTS `" + databaseName + "`"); _ = root.Close() })
	rootConfig.DBName = databaseName
	database, err := sql.Open("mysql", rootConfig.FormatDSN())
	if err != nil {
		t.Fatal("open isolated disposable database:", err)
	}
	database.SetMaxOpenConns(6)
	t.Cleanup(func() { _ = database.Close() })
	if err := database.Ping(); err != nil {
		t.Fatal("ping disposable MySQL:", err)
	}
	var tenantDatabaseName string
	if err := database.QueryRow("SELECT DATABASE()").Scan(&tenantDatabaseName); err != nil {
		t.Fatal("read disposable database name:", err)
	}
	runPurchaseApprovalPolicyAndDomainTransition(t, database, db.Resolve("mysql"), tenantDatabaseName, func() (*sql.DB, error) {
		reopened, openErr := sql.Open("mysql", rootConfig.FormatDSN())
		if openErr == nil {
			reopened.SetMaxOpenConns(6)
		}
		return reopened, openErr
	})
}

func TestPostgresPurchaseApprovalPolicyAndDomainTransitionCommitTogether(t *testing.T) {
	dsn := os.Getenv("KORA_PROCESS_DISPOSABLE_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set KORA_PROCESS_DISPOSABLE_POSTGRES_DSN to a disposable PostgreSQL service")
	}
	root, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal("open disposable PostgreSQL:", err)
	}
	if err := root.Ping(); err != nil {
		_ = root.Close()
		t.Fatal("ping disposable PostgreSQL:", err)
	}
	schemaName := fmt.Sprintf("kora_purchase_process_%x", time.Now().UnixNano())
	if _, err := root.Exec(`CREATE SCHEMA "` + schemaName + `"`); err != nil {
		_ = root.Close()
		t.Fatal("create isolated disposable schema:", err)
	}
	t.Cleanup(func() { _, _ = root.Exec(`DROP SCHEMA "` + schemaName + `" CASCADE`); _ = root.Close() })
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal("parse disposable PostgreSQL DSN:", err)
	}
	query := parsed.Query()
	query.Set("search_path", schemaName)
	parsed.RawQuery = query.Encode()
	database, err := sql.Open("postgres", parsed.String())
	if err != nil {
		t.Fatal("open isolated PostgreSQL schema:", err)
	}
	defer database.Close()
	database.SetMaxOpenConns(6)
	runPurchaseApprovalPolicyAndDomainTransition(t, database, db.Resolve("postgres"), schemaName, func() (*sql.DB, error) {
		reopened, openErr := sql.Open("postgres", parsed.String())
		if openErr == nil {
			reopened.SetMaxOpenConns(6)
		}
		return reopened, openErr
	})
}

func TestLibSQLRemotePurchaseApprovalPolicyAndDomainTransitionCommitTogether(t *testing.T) {
	dsn := os.Getenv("KORA_PROCESS_DISPOSABLE_LIBSQL_DSN")
	if dsn == "" {
		t.Skip("set KORA_PROCESS_DISPOSABLE_LIBSQL_DSN to a fresh disposable LibSQL HTTP database")
	}
	database, err := sql.Open("libsql", dsn)
	if err != nil {
		t.Fatal("open disposable LibSQL HTTP endpoint:", err)
	}
	defer database.Close()
	database.SetMaxOpenConns(6)
	runPurchaseApprovalPolicyAndDomainTransition(t, database, db.Resolve("libsql"), "", func() (*sql.DB, error) {
		reopened, openErr := sql.Open("libsql", dsn)
		if openErr == nil {
			reopened.SetMaxOpenConns(6)
		}
		return reopened, openErr
	})
}

func runPurchaseApprovalPolicyAndDomainTransition(t *testing.T, database *sql.DB, dialect db.Dialect, databaseName string, reopen func() (*sql.DB, error)) {
	t.Helper()
	if err := database.Ping(); err != nil {
		t.Fatalf("ping disposable %s database: %v", dialect.DriverName(), err)
	}
	if err := site.BootstrapSystemTables(context.Background(), database, dialect); err != nil {
		t.Fatalf("bootstrap %s Engine tables: %v", dialect.DriverName(), err)
	}
	requestDDL, err := requestsql.SchemaStatements(dialect)
	if err != nil {
		t.Fatalf("build %s request schema: %v", dialect.DriverName(), err)
	}
	for _, statement := range requestDDL {
		if _, err := database.Exec(statement); err != nil {
			t.Fatalf("apply %s request schema: %v\n%s", dialect.DriverName(), err, statement)
		}
	}
	suffix := contract.NewID()
	if len(suffix) < 16 {
		t.Fatalf("generated integration ID too short: %q", suffix)
	}
	siteName := "purchase-approval-" + suffix[:12]
	tenant := contract.TenantID("tenant-" + suffix)
	requesterID := "requester-" + suffix[:12]
	approverID := "finance-" + suffix[:12]
	managerID := "purchase-" + suffix[:12]
	var enabledValue any = true
	if dialect.DriverName() == "postgres" {
		enabledValue = 1
	}
	for principal, role := range map[string]string{requesterID: "Procurement Officer", approverID: "Finance Manager", managerID: "Purchase Manager"} {
		query := db.Rebind(dialect, `INSERT INTO _kora_user (name, site, email, password_hash, enabled, roles) VALUES (?, ?, ?, 'test', ?, ?)`)
		if _, err := database.Exec(query, principal, siteName, principal+"@example.test", enabledValue, role); err != nil {
			t.Fatalf("insert disposable %s tenant user %q: %v", dialect.DriverName(), principal, err)
		}
	}
	registry := doctype.NewRegistry()
	dt := &doctype.DocType{Name: "Purchase Request", Module: "Purchasing", Fields: []doctype.Field{
		{Fieldname: "status", Fieldtype: "Data"}, {Fieldname: "estimated_total", Fieldtype: "Currency"},
	}}
	registry.LoadFull([]*doctype.DocType{dt}, []*doctype.Role{{Name: "Procurement Officer"}, {Name: "Finance Manager"}, {Name: "Purchase Manager"}}, []*doctype.Permission{
		{Doctype: dt.Name, Role: "Procurement Officer", Read: true},
		{Doctype: dt.Name, Role: "Finance Manager", Read: true, Submit: true},
		{Doctype: dt.Name, Role: "Purchase Manager", Read: true, Submit: true},
		{Doctype: applicationworkflow.DefinitionResource, Role: "Procurement Officer", Read: true},
		{Doctype: applicationworkflow.InstanceResource, Role: "Procurement Officer", Create: true},
		{Doctype: applicationworkflow.DefinitionResource, Role: "Purchase Manager", Read: true, Write: true},
		{Doctype: applicationworkflow.InstanceResource, Role: "Purchase Manager", Create: true},
	})
	makeStatusDefinition := func(id, action, to string, roles []string, condition string, docStatus int) (contract.WorkflowDefinition, error) {
		input, err := json.Marshal(processworkflow.StatusActionConfig{StateField: "status", InitialState: "Submitted", StatusTransition: processworkflow.StatusTransition{Action: action, From: "Submitted", To: to, AllowedRoles: roles, Condition: condition, DocStatus: docStatus}})
		if err != nil {
			return contract.WorkflowDefinition{}, err
		}
		return contract.WorkflowDefinition{ID: contract.DefinitionID(id), Version: 1, Entry: "transition",
			Trigger: &contract.RecordTrigger{Kind: "record.updated", Table: dt.Name, WatchedFields: []string{"status"}},
			Steps: []contract.StepDefinition{
				{ID: "transition", Kind: contract.StepAction, Action: &contract.ActionSpec{Name: "record.update_status", Version: 1, InputSchema: "kora.record.update_status", Input: input, Next: "finish"}},
				{ID: "finish", Kind: contract.StepEnd, End: &contract.EndSpec{Outcome: "complete"}},
			}}, nil
	}
	approveStatusDefinition, err := makeStatusDefinition("purchase-request-approve-"+suffix[:12], purchasing.ApprovalAction, "Approved", []string{"Finance Manager", "Purchase Manager"}, "doc.estimated_total > 0", 0)
	if err != nil {
		t.Fatal("encode purchase approval status transition:", err)
	}
	rejectStatusDefinition, err := makeStatusDefinition("purchase-request-reject-"+suffix[:12], "Reject", "Rejected", []string{"Purchase Manager"}, "", 1)
	if err != nil {
		t.Fatal("encode purchase rejection status transition:", err)
	}
	definitionStore, err := workflowsql.NewDefinitionStore(database, dialect)
	if err != nil {
		t.Fatal("construct v2 lifecycle store:", err)
	}
	if err := definitionStore.Ensure(context.Background()); err != nil {
		t.Fatal("ensure v2 lifecycle store:", err)
	}
	migrationActor, err := contract.ParsePrincipalID("system-purchase-workflow-migration")
	if err != nil {
		t.Fatal(err)
	}
	if err := definitionStore.PublishImportedBatch(context.Background(), tenant, migrationActor, []contract.WorkflowDefinition{approveStatusDefinition, rejectStatusDefinition}, time.Now().UTC()); err != nil {
		t.Fatal("publish Purchase Request status workflows:", err)
	}
	if err := schema.MigrateSiteFromRegistry(database, databaseName, registry, dialect); err != nil {
		t.Fatalf("migrate %s Purchase Request: %v", dialect.DriverName(), err)
	}
	table := dialect.QuoteIdent(dt.RawTableName())
	nameColumn, ownerColumn := dialect.QuoteIdent("name"), dialect.QuoteIdent("owner")
	insertSubject := db.Rebind(dialect, `INSERT INTO `+table+` (`+nameColumn+`, `+ownerColumn+`, `+dialect.QuoteIdent("status")+`, `+dialect.QuoteIdent("estimated_total")+`, `+dialect.QuoteIdent("doc_status")+`, `+dialect.QuoteIdent("revision")+`) VALUES (?, ?, 'Submitted', 500, 0, 1)`)
	requestName := "PR-" + suffix[:12]
	if _, err := database.Exec(insertSubject, requestName, requesterID); err != nil {
		t.Fatalf("insert disposable %s Purchase Request: %v", dialect.DriverName(), err)
	}
	directAuthorizer, err := requestsql.NewDirectTaskAuthorizer(database, dialect)
	if err != nil {
		t.Fatal("construct direct-task authorizer:", err)
	}
	approvalStore, err := purchasingsql.New(database, dialect)
	if err != nil {
		t.Fatal("construct purchasing policy store:", err)
	}
	authorizer, err := purchasing.NewRequestAuthorizer(directAuthorizer, approvalStore, registry, definitionStore)
	if err != nil {
		t.Fatal("construct production request authorizer:", err)
	}
	requestStore, err := requestsql.New(database, dialect)
	if err != nil {
		t.Fatal("construct request SQL store:", err)
	}
	requestService, err := requests.NewService(requestStore, authorizer, requests.ParticipantProjector{}, requests.NoResponseSchemas{}, mysqlApprovalClock{}, mysqlApprovalIDs{})
	if err != nil {
		t.Fatal("construct request service:", err)
	}
	workflowPrincipal := "workflow-service-" + suffix[:12]
	workflowExecution, err := policy.NewVerifiedExecutionWithGrants(tenant, contract.ActorContext{PrincipalID: workflowPrincipal,
		PrincipalType: contract.PrincipalService, Site: siteName, AuthenticatedAt: time.Now().UTC()},
		[]policy.Grant{{Resource: purchasing.WorkflowApprovalSubmitGrant, Operation: "submit"}})
	if err != nil {
		t.Fatal("create verified workflow identity:", err)
	}
	workflowRequestName := requestName + "-workflow"
	if _, err := database.Exec(insertSubject, workflowRequestName, requesterID); err != nil {
		t.Fatalf("insert disposable %s workflow Purchase Request: %v", dialect.DriverName(), err)
	}
	workflowSource := contract.RequestSource{InstanceID: contract.InstanceID(contract.NewID()), StepID: "approval"}
	workflowCommand := contract.SubmitRequest{Meta: contract.CommandMeta{IdempotencyKey: contract.IdempotencyKey("workflow-submit-" + suffix[:12])},
		Kind: contract.RequestApproval, PolicyID: purchasing.ApprovalPolicyID,
		Subject: &contract.SubjectRef{Kind: dt.Name, ID: workflowRequestName, Revision: "1"}, Title: "Workflow approval"}
	preparedWorkflowRequest, err := requestService.PrepareWorkflowSubmit(context.Background(), workflowExecution, workflowCommand, workflowSource)
	if err != nil {
		t.Fatalf("prepare workflow-originated %s approval: %v", dialect.DriverName(), err)
	}
	workflowTx, err := database.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin workflow request transaction: %v", err)
	}
	workflowRecord, err := preparedWorkflowRequest.CommitInTx(context.Background(), workflowTx)
	if err != nil {
		_ = workflowTx.Rollback()
		t.Fatalf("persist workflow-originated %s approval: %v", dialect.DriverName(), err)
	}
	if err := workflowTx.Commit(); err != nil {
		t.Fatalf("commit workflow-originated approval: %v", err)
	}
	if workflowRecord.Requester != contract.PrincipalID(workflowPrincipal) || workflowRecord.Source == nil || *workflowRecord.Source != workflowSource || len(workflowRecord.Assignments) != 2 {
		t.Fatalf("workflow approval lost trusted source, service identity or live reviewer assignments: %+v", workflowRecord)
	}
	requesterExecution, err := policy.NewVerifiedExecution(tenant, contract.ActorContext{PrincipalID: requesterID, PrincipalType: contract.PrincipalHuman, SubjectUserID: requesterID, Site: siteName, AuthenticatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal("create verified requester:", err)
	}
	command := contract.SubmitRequest{Meta: contract.CommandMeta{IdempotencyKey: contract.IdempotencyKey("submit-" + suffix[:16])}, Kind: contract.RequestApproval,
		PolicyID: purchasing.ApprovalPolicyID, Subject: &contract.SubjectRef{Kind: dt.Name, ID: requestName, Revision: "1"}, Title: "Approve purchase"}
	view, err := requestService.Submit(context.Background(), requesterExecution, command)
	if err != nil || len(view.Assignments) != 2 {
		t.Fatalf("submit governed approval: view=%+v err=%v", view, err)
	}
	approverExecution, err := policy.NewVerifiedExecution(tenant, contract.ActorContext{PrincipalID: approverID, PrincipalType: contract.PrincipalHuman, SubjectUserID: approverID, Site: siteName, Roles: []string{"Finance Manager"}, AuthenticatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal("create verified approver:", err)
	}
	workAdmission, err := dbaccess.NewAdmission(2, 2, time.Second)
	if err != nil {
		t.Fatal("create active-work admission:", err)
	}
	transactionOpener, err := NewTenantTransactionOpener(tenant, database, workAdmission)
	if err != nil {
		t.Fatal("create tenant transaction opener:", err)
	}
	var financeAssignment, managerAssignment contract.AssignmentView
	for _, assignment := range view.Assignments {
		switch assignment.Assignee.ID {
		case approverID:
			financeAssignment = assignment
		case managerID:
			managerAssignment = assignment
		}
	}
	if financeAssignment.ID == "" || managerAssignment.ID == "" || len(financeAssignment.AllowedResponses) != 1 || financeAssignment.AllowedResponses[0] != contract.ResponseApprove {
		t.Fatalf("workflow did not project role-specific approval responses: %+v", view.Assignments)
	}
	approvalCommand := contract.RespondRequest{Meta: contract.CommandMeta{IdempotencyKey: contract.IdempotencyKey("approve-" + suffix[:16]), ExpectedRevision: view.Revision},
		AssignmentID: financeAssignment.ID, Response: contract.ResponseApprove}
	makeCoordinator := func(action string) *Coordinator {
		workflowKernel := kernel.New(dialect, outbox.NewSQLWriter(dialect))
		workflowKernel.WorkflowStatusPolicies = definitionStore
		coordinator, err := NewCoordinator(requestService, workflowKernel, registry, transactionOpener,
			map[string]WorkflowApproval{purchasing.ApprovalPolicyID: {SubjectKind: purchasing.PurchaseRequest, Action: action, RejectAction: "Reject"}})
		if err != nil {
			t.Fatal("construct approval coordinator:", err)
		}
		return coordinator
	}
	if _, err := makeCoordinator("missing-action").ApproveAndApply(context.Background(), approverExecution, view.ID, approvalCommand); err == nil {
		t.Fatal("invalid workflow action unexpectedly committed")
	}
	var decisions int
	if err := database.QueryRow(db.Rebind(dialect, "SELECT COUNT(*) FROM _kora_request_decision WHERE tenant_id = ? AND request_id = ?"), string(tenant), string(view.ID)).Scan(&decisions); err != nil || decisions != 0 {
		t.Fatalf("%s failed domain action left decisions=%d err=%v", dialect.DriverName(), decisions, err)
	}
	receipt, err := makeCoordinator(purchasing.ApprovalAction).ApproveAndApply(context.Background(), approverExecution, view.ID, approvalCommand)
	if err != nil || receipt.Status != contract.RequestResolved || receipt.Outcome == nil || *receipt.Outcome != contract.OutcomeApproved {
		t.Fatalf("approved Purchase Request: receipt=%+v err=%v", receipt, err)
	}
	var state string
	if err := database.QueryRow(db.Rebind(dialect, "SELECT "+dialect.QuoteIdent("status")+" FROM "+table+" WHERE "+nameColumn+" = ?"), requestName).Scan(&state); err != nil || state != "Approved" {
		t.Fatalf("%s domain state=%q err=%v, want Approved", dialect.DriverName(), state, err)
	}
	var operationCount, outboxCount int
	if err := database.QueryRow(db.Rebind(dialect, "SELECT COUNT(*) FROM _kora_operation_audit WHERE site = ? AND doc_name = ? AND command_name = ?"), siteName, requestName, kernel.CommandWorkflowStatusTransition).Scan(&operationCount); err != nil {
		t.Fatal("count workflow audit:", err)
	}
	if err := database.QueryRow(db.Rebind(dialect, "SELECT COUNT(*) FROM _kora_outbox WHERE site = ? AND aggregate_id = ? AND event_type = ?"), siteName, requestName, outbox.EventTypeName(dt.Name, eventstream.EventUpdate)).Scan(&outboxCount); err != nil {
		t.Fatal("count domain outbox:", err)
	}
	if operationCount != 1 || outboxCount != 1 {
		t.Fatalf("domain effects audit=%d record-trigger events=%d, want one each", operationCount, outboxCount)
	}
	rejectedName := "PR-R-" + suffix[:10]
	insertRejectedSubject := db.Rebind(dialect, `INSERT INTO `+table+` (`+nameColumn+`, `+ownerColumn+`, `+dialect.QuoteIdent("status")+`, `+dialect.QuoteIdent("estimated_total")+`, `+dialect.QuoteIdent("doc_status")+`, `+dialect.QuoteIdent("revision")+`) VALUES (?, ?, 'Submitted', 250, 0, 1)`)
	if _, err := database.Exec(insertRejectedSubject, rejectedName, requesterID); err != nil {
		t.Fatalf("insert rejection Purchase Request: %v", err)
	}
	rejectionCommand := command
	rejectionCommand.Meta.IdempotencyKey = contract.IdempotencyKey("submit-reject-" + suffix[:12])
	rejectionCommand.Subject = &contract.SubjectRef{Kind: dt.Name, ID: rejectedName, Revision: "1"}
	rejectedView, err := requestService.Submit(context.Background(), requesterExecution, rejectionCommand)
	if err != nil {
		t.Fatal("submit rejection approval:", err)
	}
	var purchaseAssignmentID contract.AssignmentID
	for _, assignment := range rejectedView.Assignments {
		if assignment.Assignee.ID == managerID {
			purchaseAssignmentID = assignment.ID
			if len(assignment.AllowedResponses) != 2 || assignment.AllowedResponses[1] != contract.ResponseReject {
				t.Fatalf("Purchase Manager response options=%v, want approve and reject", assignment.AllowedResponses)
			}
		}
	}
	if purchaseAssignmentID == "" {
		t.Fatalf("Purchase Manager assignment missing: %+v", rejectedView.Assignments)
	}
	managerExecution, err := policy.NewVerifiedExecution(tenant, contract.ActorContext{PrincipalID: managerID, PrincipalType: contract.PrincipalHuman, SubjectUserID: managerID, Site: siteName, Roles: []string{"Purchase Manager"}, AuthenticatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal("create verified Purchase Manager:", err)
	}
	rejectCommand := contract.RespondRequest{Meta: contract.CommandMeta{IdempotencyKey: contract.IdempotencyKey("reject-" + suffix[:16]), ExpectedRevision: rejectedView.Revision}, AssignmentID: purchaseAssignmentID, Response: contract.ResponseReject}
	rejectReceipt, err := makeCoordinator(purchasing.ApprovalAction).ApproveAndApply(context.Background(), managerExecution, rejectedView.ID, rejectCommand)
	if err != nil || rejectReceipt.Status != contract.RequestResolved || rejectReceipt.Outcome == nil || *rejectReceipt.Outcome != contract.OutcomeRejected {
		t.Fatalf("reject Purchase Request: receipt=%+v err=%v", rejectReceipt, err)
	}
	if err := database.QueryRow(db.Rebind(dialect, "SELECT "+dialect.QuoteIdent("status")+" FROM "+table+" WHERE "+nameColumn+" = ?"), rejectedName).Scan(&state); err != nil || state != "Rejected" {
		t.Fatalf("%s rejected domain state=%q err=%v, want Rejected", dialect.DriverName(), state, err)
	}

	staleName := "PR-S-" + suffix[:10]
	if _, err := database.Exec(insertSubject, staleName, requesterID); err != nil {
		t.Fatalf("insert stale-subject Purchase Request: %v", err)
	}
	staleCommand := command
	staleCommand.Meta.IdempotencyKey = contract.IdempotencyKey("submit-stale-" + suffix[:12])
	staleCommand.Subject = &contract.SubjectRef{Kind: dt.Name, ID: staleName, Revision: "1"}
	staleView, err := requestService.Submit(context.Background(), requesterExecution, staleCommand)
	if err != nil {
		t.Fatalf("submit stale-subject approval: %v", err)
	}
	var staleAssignmentID contract.AssignmentID
	for _, assignment := range staleView.Assignments {
		if assignment.Assignee.ID == approverID {
			staleAssignmentID = assignment.ID
		}
	}
	if staleAssignmentID == "" {
		t.Fatalf("%s stale-subject reviewer assignment missing: %+v", dialect.DriverName(), staleView.Assignments)
	}
	if _, err := database.Exec(db.Rebind(dialect, "UPDATE "+table+" SET "+dialect.QuoteIdent("estimated_total")+" = ?, "+dialect.QuoteIdent("revision")+" = 2 WHERE "+nameColumn+" = ?"), 777, staleName); err != nil {
		t.Fatalf("edit approved Purchase Request subject: %v", err)
	}
	staleResponse := contract.RespondRequest{Meta: contract.CommandMeta{IdempotencyKey: contract.IdempotencyKey("respond-stale-" + suffix[:12]), ExpectedRevision: staleView.Revision},
		AssignmentID: staleAssignmentID, Response: contract.ResponseApprove}
	_, err = makeCoordinator(purchasing.ApprovalAction).ApproveAndApply(context.Background(), approverExecution, staleView.ID, staleResponse)
	var staleErr *contract.Error
	if !errors.As(err, &staleErr) || staleErr.Type != contract.CodeStaleSubject {
		t.Fatalf("%s changed subject response error=%v, want stale subject", dialect.DriverName(), err)
	}
	if err := database.QueryRow(db.Rebind(dialect, "SELECT COUNT(*) FROM _kora_request_decision WHERE tenant_id = ? AND request_id = ?"), string(tenant), string(staleView.ID)).Scan(&decisions); err != nil || decisions != 0 {
		t.Fatalf("%s changed subject left decisions=%d err=%v", dialect.DriverName(), decisions, err)
	}
	if err := database.QueryRow(db.Rebind(dialect, "SELECT "+dialect.QuoteIdent("status")+" FROM "+table+" WHERE "+nameColumn+" = ?"), staleName).Scan(&state); err != nil || state != "Submitted" {
		t.Fatalf("%s stale-subject domain state=%q err=%v, want Submitted", dialect.DriverName(), state, err)
	}

	revokedName := "PR-V-" + suffix[:10]
	if _, err := database.Exec(insertSubject, revokedName, requesterID); err != nil {
		t.Fatalf("insert revocation Purchase Request: %v", err)
	}
	revokedCommand := command
	revokedCommand.Meta.IdempotencyKey = contract.IdempotencyKey("submit-revoke-" + suffix[:12])
	revokedCommand.Subject = &contract.SubjectRef{Kind: dt.Name, ID: revokedName, Revision: "1"}
	revokedView, err := requestService.Submit(context.Background(), requesterExecution, revokedCommand)
	if err != nil {
		t.Fatalf("submit revocation approval: %v", err)
	}
	var revokedAssignmentID contract.AssignmentID
	for _, assignment := range revokedView.Assignments {
		if assignment.Assignee.ID == approverID {
			revokedAssignmentID = assignment.ID
		}
	}
	if revokedAssignmentID == "" {
		t.Fatalf("%s revoked-user assignment missing: %+v", dialect.DriverName(), revokedView.Assignments)
	}
	revokeTx, err := database.Begin()
	if err != nil {
		t.Fatalf("begin membership revocation: %v", err)
	}
	if _, err := revokeTx.Exec(db.Rebind(dialect, `UPDATE _kora_user SET enabled = ? WHERE site = ? AND name = ?`), db.BoolParam(dialect, false), siteName, approverID); err != nil {
		_ = revokeTx.Rollback()
		t.Fatalf("revoke purchasing reviewer: %v", err)
	}
	if err := configstore.BumpAuthorizationRevisionTx(revokeTx, dialect, false, true); err != nil {
		_ = revokeTx.Rollback()
		t.Fatalf("advance membership revision: %v", err)
	}
	if err := revokeTx.Commit(); err != nil {
		t.Fatalf("commit membership revocation: %v", err)
	}
	revokedExecution, err := policy.NewVerifiedExecution(tenant, contract.ActorContext{PrincipalID: approverID, PrincipalType: contract.PrincipalHuman, SubjectUserID: approverID, Site: siteName, Roles: []string{"Finance Manager"}, AuthenticatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal("create revoked approver identity:", err)
	}
	revokedResponse := contract.RespondRequest{Meta: contract.CommandMeta{IdempotencyKey: contract.IdempotencyKey("respond-revoke-" + suffix[:12]), ExpectedRevision: revokedView.Revision},
		AssignmentID: revokedAssignmentID, Response: contract.ResponseApprove}
	_, err = makeCoordinator(purchasing.ApprovalAction).ApproveAndApply(context.Background(), revokedExecution, revokedView.ID, revokedResponse)
	var contractErr *contract.Error
	if !errors.As(err, &contractErr) || contractErr.Type != contract.CodePermissionDenied {
		t.Fatalf("%s revoked reviewer response error=%v, want permission denied", dialect.DriverName(), err)
	}
	if err := database.QueryRow(db.Rebind(dialect, "SELECT COUNT(*) FROM _kora_request_decision WHERE tenant_id = ? AND request_id = ?"), string(tenant), string(revokedView.ID)).Scan(&decisions); err != nil || decisions != 0 {
		t.Fatalf("%s revoked reviewer left decisions=%d err=%v", dialect.DriverName(), decisions, err)
	}
	workflowSubjectName := "PR-WF-" + suffix[:10]
	if _, err := database.Exec(insertSubject, workflowSubjectName, requesterID); err != nil {
		t.Fatalf("insert Purchase Request for production workflow test: %v", err)
	}
	runProductionPurchaseApprovalWorkflowWait(t, context.Background(), database, dialect, tenant, siteName, suffix, workflowSubjectName, managerExecution, registry, requestService, makeCoordinator, reopen)
}

func runProductionPurchaseApprovalWorkflowWait(t *testing.T, ctx context.Context, database *sql.DB, dialect db.Dialect, tenant contract.TenantID, siteName, suffix, subjectName string, manager policy.Execution, registry *doctype.Registry, requestService *requests.Service, makeCoordinator func(string) *Coordinator, reopen func() (*sql.DB, error)) {
	t.Helper()
	definitions, err := workflowsql.NewDefinitionStore(database, dialect)
	if err != nil {
		t.Fatal("construct workflow definition store:", err)
	}
	if err := definitions.Ensure(ctx); err != nil {
		t.Fatal("ensure durable workflow schema:", err)
	}
	instances, err := workflowsql.NewInstanceStore(database, dialect)
	if err != nil {
		t.Fatal("construct workflow instance store:", err)
	}
	executions, err := workflowsql.NewExecutionStore(database, dialect)
	if err != nil {
		t.Fatal("construct workflow execution store:", err)
	}
	authorizationStore, err := workflowsql.NewAuthorizationStore(database, dialect, registry)
	if err != nil {
		t.Fatal("construct live workflow authorization store:", err)
	}
	workflowAuthorizer, err := applicationworkflow.NewAuthorizer(authorizationStore, registry)
	if err != nil {
		t.Fatal("construct production workflow authorizer:", err)
	}
	catalog := processworkflow.NewCatalog()
	if err := catalog.Freeze(); err != nil {
		t.Fatal("freeze workflow action catalog:", err)
	}
	schemas, waits := integrationWorkflowSchemas{}, restartWorkflowWaits{}
	definitionService, err := processworkflow.NewService(definitions, workflowAuthorizer, catalog, schemas, waits, nil)
	if err != nil {
		t.Fatal("construct authorized workflow definition service:", err)
	}
	definition := contract.WorkflowDefinition{ID: contract.DefinitionID("purchase-approval-" + suffix[:12]), Version: 1, Entry: "approval", Steps: []contract.StepDefinition{
		{ID: "approval", Kind: contract.StepHuman, Human: &contract.HumanSpec{Request: contract.HumanRequestTemplate{Kind: contract.RequestApproval, Subject: contract.SubjectInstance,
			Title: "Approve Purchase Request", Message: "Review this Purchase Request", PolicyID: purchasing.ApprovalPolicyID, DueAfterSeconds: 3600},
			Outcomes:  map[contract.ResponseKind]contract.StepID{contract.ResponseApprove: "approved", contract.ResponseReject: "rejected", contract.ResponseReturn: "rejected"},
			OnExpired: "rejected", OnCancelled: "rejected", OnStale: "rejected"}},
		{ID: "approved", Kind: contract.StepEnd, End: &contract.EndSpec{Outcome: "approved"}},
		{ID: "rejected", Kind: contract.StepEnd, End: &contract.EndSpec{Outcome: "not-approved"}},
	}}
	if _, err := definitionService.Publish(ctx, manager, definition, contract.IdempotencyKey("publish-approval-flow-"+suffix[:12])); err != nil {
		t.Fatal("publish workflow through current authorization:", err)
	}
	instanceService, err := processworkflow.NewInstanceService(definitions, instances, workflowAuthorizer, catalog, schemas, waits, nil, nil)
	if err != nil {
		t.Fatal("construct authorized workflow instance service:", err)
	}
	instance, err := instanceService.Start(ctx, manager, contract.StartWorkflow{Meta: contract.CommandMeta{IdempotencyKey: contract.IdempotencyKey("start-approval-flow-" + suffix[:12])},
		DefinitionID: definition.ID, DefinitionVersion: definition.Version,
		Subject: contract.SubjectRef{Kind: purchasing.PurchaseRequest, ID: subjectName, Revision: "1"}})
	if err != nil {
		t.Fatal("start workflow through current authorization:", err)
	}
	serviceExecution, err := policy.NewVerifiedExecutionWithGrants(tenant, contract.ActorContext{PrincipalID: "kora-workflow-runtime", PrincipalType: contract.PrincipalService, Site: siteName, AuthenticatedAt: time.Now().UTC()},
		[]policy.Grant{{Resource: purchasing.WorkflowApprovalSubmitGrant, Operation: "submit"}})
	if err != nil {
		t.Fatal("create narrowly granted workflow service identity:", err)
	}
	identity, err := processworkflow.NewTenantServiceIdentityProvider(tenant, serviceExecution.Actor(), []policy.Grant{{Resource: purchasing.WorkflowApprovalSubmitGrant, Operation: "submit"}})
	if err != nil {
		t.Fatal("construct workflow service identity provider:", err)
	}
	handler, err := processworkflow.NewDefinitionClaimHandler(executions, instances, definitions, catalog, schemas, identity, restartWorkflowScopes{}, expression.NewTreeEvaluator(), nil, processworkflow.HumanWaitRuntime{Requests: requestService, Store: executions, Now: time.Now})
	if err != nil {
		t.Fatal("construct production-policy human wait handler:", err)
	}
	claims, err := executions.ClaimDue(ctx, tenant, "purchase-approval-worker-1", 1, time.Minute)
	if err != nil || len(claims) != 1 || claims[0].InstanceID != instance.ID {
		t.Fatalf("claim production approval workflow = %#v, %v", claims, err)
	}
	if err := handler.HandleClaim(ctx, claims[0]); err != nil {
		t.Fatal("create workflow request through purchasing policy:", err)
	}
	var requestID, revision, assignmentID string
	if err := database.QueryRowContext(ctx, db.Rebind(dialect, `SELECT id,revision FROM _kora_request WHERE tenant_id=? AND source_instance_id=? AND source_step_id='approval'`), string(tenant), string(instance.ID)).Scan(&requestID, &revision); err != nil {
		t.Fatal("read durable production approval request:", err)
	}
	if err := database.QueryRowContext(ctx, db.Rebind(dialect, `SELECT id FROM _kora_request_assignment WHERE tenant_id=? AND request_id=? AND assignee_id=? AND status='pending'`), string(tenant), requestID, manager.Actor().PrincipalID).Scan(&assignmentID); err != nil {
		t.Fatal("find currently eligible Purchase Manager assignment:", err)
	}
	receipt, err := makeCoordinator(purchasing.ApprovalAction).ApproveAndApply(ctx, manager, contract.RequestID(requestID), contract.RespondRequest{
		Meta:         contract.CommandMeta{IdempotencyKey: contract.IdempotencyKey("respond-approval-flow-" + suffix[:12]), ExpectedRevision: contract.Revision(revision)},
		AssignmentID: contract.AssignmentID(assignmentID), Response: contract.ResponseApprove})
	if err != nil || receipt.Status != contract.RequestResolved || receipt.Outcome == nil || *receipt.Outcome != contract.OutcomeApproved {
		t.Fatalf("resolve workflow approval through domain coordinator: receipt=%+v err=%v", receipt, err)
	}
	// Drop the SQL pool after the domain decision, then rebuild application and
	// workflow services against a fresh pool before scanning durable outcomes.
	if err := database.Close(); err != nil {
		t.Fatal("close database pool before workflow recovery:", err)
	}
	database, err = reopen()
	if err != nil {
		t.Fatal("reopen database pool before workflow recovery:", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.PingContext(ctx); err != nil {
		t.Fatal("ping reopened database pool:", err)
	}
	directAuthorizer, err := requestsql.NewDirectTaskAuthorizer(database, dialect)
	if err != nil {
		t.Fatal("rebuild direct-task request policy:", err)
	}
	approvalStore, err := purchasingsql.New(database, dialect)
	if err != nil {
		t.Fatal("rebuild purchasing approval policy store:", err)
	}
	recoveredDefinitionStore, err := workflowsql.NewDefinitionStore(database, dialect)
	if err != nil {
		t.Fatal("rebuild v2 lifecycle store:", err)
	}
	requestAuthorizer, err := purchasing.NewRequestAuthorizer(directAuthorizer, approvalStore, registry, recoveredDefinitionStore)
	if err != nil {
		t.Fatal("rebuild production request policy:", err)
	}
	recoveredRequestStore, err := requestsql.New(database, dialect)
	if err != nil {
		t.Fatal("rebuild request store:", err)
	}
	requestService, err = requests.NewService(recoveredRequestStore, requestAuthorizer, requests.ParticipantProjector{}, requests.NoResponseSchemas{}, mysqlApprovalClock{}, mysqlApprovalIDs{})
	if err != nil {
		t.Fatal("rebuild production request service:", err)
	}
	recoveredAuthorizationStore, err := workflowsql.NewAuthorizationStore(database, dialect, registry)
	if err != nil {
		t.Fatal("rebuild workflow authorization store:", err)
	}
	workflowAuthorizer, err = applicationworkflow.NewAuthorizer(recoveredAuthorizationStore, registry)
	if err != nil {
		t.Fatal("rebuild production workflow authorizer:", err)
	}
	// Rebuild the workflow stores and claim handler as a newly started worker
	// would before scanning durable terminal request outcomes.
	recoveredDefinitions, err := workflowsql.NewDefinitionStore(database, dialect)
	if err != nil {
		t.Fatal("rebuild workflow definition store after decision:", err)
	}
	recoveredInstances, err := workflowsql.NewInstanceStore(database, dialect)
	if err != nil {
		t.Fatal("rebuild workflow instance store after decision:", err)
	}
	recoveredExecutions, err := workflowsql.NewExecutionStore(database, dialect)
	if err != nil {
		t.Fatal("rebuild workflow execution store after decision:", err)
	}
	instanceService, err = processworkflow.NewInstanceService(recoveredDefinitions, recoveredInstances, workflowAuthorizer, catalog, schemas, waits, nil, nil)
	if err != nil {
		t.Fatal("rebuild authorized workflow instance service:", err)
	}
	recoveredHandler, err := processworkflow.NewDefinitionClaimHandler(recoveredExecutions, recoveredInstances, recoveredDefinitions, catalog, schemas, identity, restartWorkflowScopes{}, expression.NewTreeEvaluator(), nil, processworkflow.HumanWaitRuntime{Requests: requestService, Store: recoveredExecutions, Now: time.Now})
	if err != nil {
		t.Fatal("rebuild workflow claim handler after decision:", err)
	}
	resumed, err := recoveredExecutions.ResumeTerminalHumanWaits(ctx, tenant, 10)
	if err != nil || resumed != 1 {
		t.Fatalf("recover workflow from durable approved request: resumed=%d err=%v", resumed, err)
	}
	claims, err = recoveredExecutions.ClaimDue(ctx, tenant, "purchase-approval-worker-2", 1, time.Minute)
	if err != nil || len(claims) != 1 || claims[0].StepID != "approved" {
		t.Fatalf("claim approval continuation = %#v, %v", claims, err)
	}
	if err := recoveredHandler.HandleClaim(ctx, claims[0]); err != nil {
		t.Fatal("complete workflow after domain approval:", err)
	}
	final, err := instanceService.Get(ctx, manager, instance.ID)
	if err != nil || final.Status != contract.InstanceCompleted || final.CurrentStep != "approved" {
		t.Fatalf("final authorized workflow instance=(%s,%s), err=%v", final.Status, final.CurrentStep, err)
	}
	var state string
	if err := database.QueryRowContext(ctx, db.Rebind(dialect, "SELECT "+dialect.QuoteIdent("status")+" FROM "+dialect.QuoteIdent((&doctype.DocType{Name: purchasing.PurchaseRequest}).RawTableName())+" WHERE "+dialect.QuoteIdent("name")+"=?"), subjectName).Scan(&state); err != nil || state != "Approved" {
		t.Fatalf("workflow domain subject state=%q err=%v, want Approved", state, err)
	}
}
