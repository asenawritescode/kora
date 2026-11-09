//go:build integration

package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/policy"
	"github.com/asenawritescode/kora/internal/script"
	"github.com/asenawritescode/kora/internal/site"
	"github.com/asenawritescode/kora/internal/workflow"
	workflowsql "github.com/asenawritescode/kora/internal/workflow/sqlstore"
	_ "github.com/go-sql-driver/mysql"
	"golang.org/x/crypto/bcrypt"
)

// TestEngineProcessRestartResumesTimedWorkflow verifies durable wait recovery
// across two real CLI Engine processes. It uses an isolated tmpfs-backed MySQL
// server and never connects to an existing Kora database.
func TestEngineProcessRestartResumesTimedWorkflow(t *testing.T) {
	if os.Getenv("KORA_ENGINE_WORKFLOW_PROCESS_RESTART") != "1" {
		t.Skip("set KORA_ENGINE_WORKFLOW_PROCESS_RESTART=1 to run disposable MySQL Engine restart acceptance")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is required for the disposable MySQL container")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	container := "kora-workflow-restart-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	startDB := exec.CommandContext(ctx, "docker", "run", "--rm", "-d", "--name", container,
		"--tmpfs", "/var/lib/mysql:rw,size=768m", "-e", "MYSQL_ROOT_PASSWORD=acceptance-only",
		"-e", "MYSQL_DATABASE=kora_platform", "-p", "127.0.0.1::3306", "mysql:8.4.11")
	if output, err := startDB.CombinedOutput(); err != nil {
		t.Fatalf("start disposable workflow-restart MySQL: %v (%s)", err, strings.TrimSpace(string(output)))
	}
	defer func() { _ = exec.Command("docker", "rm", "-f", container).Run() }()
	platformPort := inspectDockerPort(ctx, t, container, "3306/tcp")
	waitMySQLContainer(ctx, t, container)
	address := netJoinLoopback(platformPort)
	dsn := fmt.Sprintf("root:acceptance-only@tcp(%s)/kora_platform?parseTime=true&multiStatements=true", address)
	platform, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal("open disposable platform database:", err)
	}
	defer platform.Close()
	for {
		if err := platform.PingContext(ctx); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("disposable platform database did not accept host connections:", ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
	dialect := db.Resolve("mysql")
	if err = site.BootstrapPlatformRegistry(context.Background(), platform, dialect); err != nil {
		t.Fatal("bootstrap disposable platform directory:", err)
	}
	tenantDB := "kora_workflow_restart_" + strconv.FormatInt(time.Now().UnixNano(), 10)
	if _, err := platform.ExecContext(ctx, "CREATE DATABASE `"+tenantDB+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		t.Fatal("create disposable tenant database:", err)
	}
	defer func() {
		if _, err := platform.Exec("DROP DATABASE `" + tenantDB + "`"); err != nil {
			t.Errorf("drop disposable tenant database: %v", err)
		}
	}()
	hostname := "workflow-restart-" + strconv.FormatInt(time.Now().UnixNano(), 10) + ".example.invalid"
	registry := site.NewSQLSiteRegistry(platform, "mysql")
	if err = registry.Upsert(&site.Config{
		Hostname: hostname, DomainsList: []string{hostname}, DBType: "mysql", DBHost: "127.0.0.1",
		DBPort: mustPort(t, platformPort), DBName: tenantDB, DBUser: "root", DBPassword: "acceptance-only",
	}); err != nil {
		t.Fatal("create disposable tenant directory entry:", err)
	}
	entry, err := registry.ResolveAlias(hostname)
	if err != nil {
		t.Fatal("resolve disposable tenant directory entry:", err)
	}
	tenant, err := contract.ParseTenantID(entry.SiteID)
	if err != nil {
		t.Fatal("parse stable tenant ID:", err)
	}

	tenantDSN := fmt.Sprintf("root:acceptance-only@tcp(%s)/%s?parseTime=true&multiStatements=true", address, tenantDB)
	tenantDBConn, err := sql.Open("mysql", tenantDSN)
	if err != nil {
		t.Fatal("open disposable tenant database:", err)
	}
	tenantDBConn.SetMaxOpenConns(4)
	defer tenantDBConn.Close()
	if err = site.BootstrapSystemTables(ctx, tenantDBConn, dialect); err != nil {
		t.Fatal("bootstrap disposable tenant system tables:", err)
	}
	hookRegistry := doctype.NewRegistry()
	task := &doctype.DocType{Name: "Task", Fields: []doctype.Field{{Fieldname: "title", Fieldtype: "Data", Reqd: true}}}
	if err := site.ImportConfigFromSnapshot(tenantDBConn, hookRegistry, tenantDB, hostname, "restart lifecycle hook fixture", &doctype.ConfigSnapshot{
		DocTypes: []*doctype.DocType{task},
		Roles:    []*doctype.Role{{Name: doctype.AdminRole}},
		Permissions: []*doctype.Permission{{
			Doctype: task.Name, Role: doctype.AdminRole, Read: true, Create: true, Write: true, Delete: true,
		}},
	}, dialect, nil); err != nil {
		t.Fatal("import disposable lifecycle-hook doctype:", err)
	}
	definitions, err := workflowsql.NewDefinitionStore(tenantDBConn, dialect)
	if err != nil {
		t.Fatal("create workflow definition store:", err)
	}
	if err := definitions.Ensure(ctx); err != nil {
		t.Fatal("ensure workflow schema:", err)
	}
	definition := contract.WorkflowDefinition{
		ID: "restart_acceptance", Version: 1, Entry: "pause",
		Steps: []contract.StepDefinition{
			{ID: "pause", Kind: contract.StepWait, Wait: &contract.WaitSpec{TimeoutSeconds: 10, OnTimeout: "finish"}},
			{ID: "finish", Kind: contract.StepEnd, End: &contract.EndSpec{Outcome: "timeout"}},
		},
	}
	ref, err := definitions.PublishDefinition(ctx, tenant, definition, "acceptance_operator", "restart-definition", time.Now().UTC())
	if err != nil {
		t.Fatal("publish durable timeout definition:", err)
	}
	instances, err := workflowsql.NewInstanceStore(tenantDBConn, dialect)
	if err != nil {
		t.Fatal("create workflow instance store:", err)
	}
	instanceID := contract.InstanceID("restart_" + contract.NewID())
	if _, err := instances.StartInstance(ctx, workflow.StartInstanceInput{
		Tenant: tenant, ID: instanceID, Definition: definition, DefinitionDigest: ref.Digest,
		Subject: contract.SubjectRef{Kind: "acceptance", ID: "subject-1", Revision: "1"},
		Actor:   "acceptance_operator", IdempotencyKey: "restart-instance", Site: hostname, Now: time.Now().UTC(),
	}); err != nil {
		t.Fatal("start durable workflow instance:", err)
	}

	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve Engine source directory")
	}
	root := filepath.Dir(filepath.Dir(sourceFile))
	binary := filepath.Join(t.TempDir(), "kora-workflow-restart-acceptance")
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, ".")
	build.Dir, build.Stdout, build.Stderr = root, io.Discard, io.Discard
	if err := build.Run(); err != nil {
		t.Fatal("build Engine for process-restart acceptance:", err)
	}
	port := freeTCPPort(t)
	configDir := t.TempDir()
	engineEnv := isolatedWorkflowRestartEnvironment(dsn, platformPort, configDir,
		"engine:workflow-restart:"+strconv.FormatInt(time.Now().UnixNano(), 10), true)
	client := &http.Client{Timeout: 3 * time.Second}
	first := startCellAcceptanceEngine(t, ctx, root, binary, port, engineEnv)
	first.waitPing(t, ctx, client)
	if got := sitePathPing(client, port, entry.SiteID); got != http.StatusOK {
		first.stop(t)
		t.Fatalf("first Engine tenant ping = HTTP %d, want 200; logs=%s", got, tailAcceptanceLog(redactedCellEngineLogs(first), 8192))
	}
	waitWorkflowInstanceStatus(t, ctx, tenantDBConn, tenant, instanceID, contract.InstanceWaiting)
	var nextWake time.Time
	if err := tenantDBConn.QueryRowContext(ctx, "SELECT next_wake_at FROM _kora_workflow_instance WHERE tenant_id = ? AND id = ?", string(tenant), string(instanceID)).Scan(&nextWake); err != nil {
		t.Fatalf("read persisted workflow wake deadline: %v", err)
	}
	first.stop(t)
	// Persist a valid pending lifecycle hook after process #1 has exited. The
	// replacement Engine must discover and execute it from the tenant database.
	hookStore := &script.Store{DB: tenantDBConn, Dialect: dialect}
	if err := hookStore.EnsureAsyncHookInvocationSchema(ctx); err != nil {
		t.Fatal("ensure durable lifecycle-hook table:", err)
	}
	hookSource := contract.OperationID(contract.NewOperationID())
	hookActor := contract.ActorContext{
		PrincipalID: "acceptance_operator", SubjectUserID: "acceptance_operator", PrincipalType: contract.PrincipalHuman,
		Site: hostname, Roles: []string{doctype.AdminRole}, AuthenticatedAt: time.Now().UTC(),
	}
	hookTx, err := tenantDBConn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal("begin durable lifecycle-hook fixture transaction:", err)
	}
	if err := hookStore.AppendAsyncHookTx(ctx, hookTx, script.AsyncHookInvocation{
		TenantID: tenant, Site: hostname, SourceOperation: hookSource, ConfigRevision: 1, Sequence: 1,
		Doctype: task.Name, Event: script.EventAfterSave,
		Record: script.Record{Name: "restart-after-save", Site: hostname, ScriptType: script.TypeDocEvent,
			DocType: task.Name, Event: script.EventAfterSave, IsActive: true, TimeoutMs: 5000, Script: "return true"},
		Document: json.RawMessage(`{"name":"TASK-RESTART-1","title":"restart source"}`), Actor: hookActor,
		Grants: []policy.Grant{{Resource: task.Name, Operation: "create"}},
	}); err != nil {
		_ = hookTx.Rollback()
		t.Fatal("append durable lifecycle-hook fixture:", err)
	}
	if err := hookTx.Commit(); err != nil {
		t.Fatal("commit durable lifecycle-hook fixture:", err)
	}
	select {
	case <-ctx.Done():
		t.Fatal("workflow restart acceptance timed out before wake deadline:", ctx.Err())
	case <-time.After(time.Until(nextWake) + 500*time.Millisecond):
	}
	second := startCellAcceptanceEngine(t, ctx, root, binary, port, engineEnv)
	second.waitPing(t, ctx, client)
	if got := sitePathPing(client, port, entry.SiteID); got != http.StatusOK {
		second.stop(t)
		t.Fatalf("restarted Engine tenant ping = HTTP %d, want 200; logs=%s", got, tailAcceptanceLog(redactedCellEngineLogs(second), 8192))
	}
	waitWorkflowInstanceStatus(t, ctx, tenantDBConn, tenant, instanceID, contract.InstanceCompleted)
	waitDurableLifecycleHookStatus(t, ctx, tenantDBConn, tenant, hookSource, "succeeded")
	second.stop(t)
	t.Logf("Engine restart recovered tenant %s workflow %s (%s v%d) and lifecycle hook %s", tenant, instanceID, definition.ID, definition.Version, hookSource)
}

// TestEngineProcessRestartResumesGovernedHumanApproval proves a real Engine
// process can persist a governed Purchase Request approval, accept the
// authorized human decision, stop before workflow continuation, then resume
// the workflow and finish the protected domain transition after restart.
func TestEngineProcessRestartResumesGovernedHumanApproval(t *testing.T) {
	if os.Getenv("KORA_ENGINE_WORKFLOW_PROCESS_RESTART") != "1" {
		t.Skip("set KORA_ENGINE_WORKFLOW_PROCESS_RESTART=1 to run disposable MySQL Engine restart acceptance")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is required for the disposable MySQL container")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	container := "kora-human-workflow-restart-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	startDB := exec.CommandContext(ctx, "docker", "run", "--rm", "-d", "--name", container,
		"--tmpfs", "/var/lib/mysql:rw,size=768m", "-e", "MYSQL_ROOT_PASSWORD=acceptance-only",
		"-e", "MYSQL_DATABASE=kora_platform", "-p", "127.0.0.1::3306", "mysql:8.4.11")
	if output, err := startDB.CombinedOutput(); err != nil {
		t.Fatalf("start disposable human-workflow MySQL: %v (%s)", err, strings.TrimSpace(string(output)))
	}
	defer func() { _ = exec.Command("docker", "rm", "-f", container).Run() }()
	platformPort := inspectDockerPort(ctx, t, container, "3306/tcp")
	waitMySQLContainer(ctx, t, container)
	address := netJoinLoopback(platformPort)
	dsn := fmt.Sprintf("root:acceptance-only@tcp(%s)/kora_platform?parseTime=true&multiStatements=true", address)
	platform, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal("open disposable platform database:", err)
	}
	defer platform.Close()
	for {
		if err := platform.PingContext(ctx); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("disposable platform database did not accept host connections:", ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
	dialect := db.Resolve("mysql")
	if err = site.BootstrapPlatformRegistry(context.Background(), platform, dialect); err != nil {
		t.Fatal("bootstrap disposable platform directory:", err)
	}
	tenantDB := "kora_human_workflow_" + strconv.FormatInt(time.Now().UnixNano(), 10)
	if _, err := platform.ExecContext(ctx, "CREATE DATABASE `"+tenantDB+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		t.Fatal("create disposable tenant database:", err)
	}
	defer func() {
		if _, err := platform.Exec("DROP DATABASE `" + tenantDB + "`"); err != nil {
			t.Errorf("drop disposable tenant database: %v", err)
		}
	}()
	hostname := "human-workflow-" + strconv.FormatInt(time.Now().UnixNano(), 10) + ".example.invalid"
	registry := site.NewSQLSiteRegistry(platform, "mysql")
	if err = registry.Upsert(&site.Config{
		Hostname: hostname, DomainsList: []string{hostname}, DBType: "mysql", DBHost: "127.0.0.1",
		DBPort: mustPort(t, platformPort), DBName: tenantDB, DBUser: "root", DBPassword: "acceptance-only",
	}); err != nil {
		t.Fatal("create disposable tenant directory entry:", err)
	}
	entry, err := registry.ResolveAlias(hostname)
	if err != nil {
		t.Fatal("resolve disposable tenant directory entry:", err)
	}
	tenant, err := contract.ParseTenantID(entry.SiteID)
	if err != nil {
		t.Fatal("parse stable tenant ID:", err)
	}
	tenantDSN := fmt.Sprintf("root:acceptance-only@tcp(%s)/%s?parseTime=true&multiStatements=true", address, tenantDB)
	tenantDatabase, err := sql.Open("mysql", tenantDSN)
	if err != nil {
		t.Fatal("open disposable tenant database:", err)
	}
	tenantDatabase.SetMaxOpenConns(8)
	defer tenantDatabase.Close()
	if err = site.BootstrapSystemTables(ctx, tenantDatabase, dialect); err != nil {
		t.Fatal("bootstrap disposable tenant tables:", err)
	}
	suffix := contract.NewID()
	if err := seedHumanWorkflowTenant(ctx, tenantDatabase, dialect, tenantDB, hostname, "PR-"+suffix[:12]); err != nil {
		t.Fatal("seed disposable Purchase Request policy and subject:", err)
	}

	procurementName, managerName := "procurement-"+suffix[:10], "manager-"+suffix[:10]
	procurementEmail, managerEmail := procurementName+"@example.invalid", managerName+"@example.invalid"
	password := "Acceptance-only-73!"
	procurementHash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatal("hash disposable procurement password:", err)
	}
	managerHash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatal("hash disposable manager password:", err)
	}
	for _, user := range []struct{ name, email, hash, role string }{
		{procurementName, procurementEmail, string(procurementHash), "Procurement Officer"},
		{managerName, managerEmail, string(managerHash), "Purchase Manager"},
	} {
		if _, err := tenantDatabase.ExecContext(ctx, `INSERT INTO _kora_user (name,site,email,password_hash,full_name,enabled,email_verified_at,roles) VALUES (?,?,?,?,?,1,CURRENT_TIMESTAMP(6),?)`,
			user.name, hostname, user.email, user.hash, user.name, user.role); err != nil {
			t.Fatalf("insert disposable human user %q: %v", user.name, err)
		}
	}

	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve Engine source directory")
	}
	root := filepath.Dir(filepath.Dir(sourceFile))
	binary := filepath.Join(t.TempDir(), "kora-human-workflow-restart-acceptance")
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, ".")
	build.Dir, build.Stdout, build.Stderr = root, io.Discard, io.Discard
	if err := build.Run(); err != nil {
		t.Fatal("build Engine for human-workflow restart acceptance:", err)
	}
	port := freeTCPPort(t)
	cloudRequestStarted := make(chan struct{}, 1)
	cloudResponseStatus := make(chan int, 1)
	cloudRelease := make(chan struct{})
	var releaseCloud sync.Once
	cloudServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case cloudRequestStarted <- struct{}{}:
		default:
		}
		<-cloudRelease
		w.WriteHeader(http.StatusServiceUnavailable)
		cloudResponseStatus <- http.StatusServiceUnavailable
	}))
	defer cloudServer.Close()
	defer releaseCloud.Do(func() { close(cloudRelease) })
	engineEnv := isolatedWorkflowRestartEnvironment(dsn, platformPort, t.TempDir(), "engine:human-workflow-restart:"+suffix[:12])
	engineEnv = append(engineEnv,
		"KORA_CLOUD_EVENTS_URL="+cloudServer.URL,
		"KORA_CLOUD_ACCOUNT_ID=offline-cloud-acceptance",
	)
	client := newWorkflowRestartHTTPClient(t)
	first := startCellAcceptanceEngine(t, ctx, root, binary, port, engineEnv)
	first.waitPing(t, ctx, client)
	if got, body := sitePathPingDetails(client, port, entry.SiteID); got != http.StatusOK {
		first.stop(t)
		t.Fatalf("initial tenant activation = HTTP %d, want 200; body=%s; logs=%s", got, body, tailAcceptanceLog(redactedCellEngineLogs(first), 8192))
	}

	if err := loginWorkflowRestartUser(ctx, client, port, entry.SiteID, procurementEmail, password); err != nil {
		first.stop(t)
		t.Fatalf("authenticate workflow publisher/starter: %v; Engine logs=%s", err, tailAcceptanceLog(redactedCellEngineLogs(first), 8192))
	}
	definition := purchaseApprovalRestartDefinition(suffix[:12])
	definitionURL := fmt.Sprintf("http://127.0.0.1:%d/s/%s/api/v2/workflow-definitions", port, entry.SiteID)
	var published struct {
		Data contract.DefinitionVersionRef `json:"data"`
	}
	if status, body, err := postWorkflowRestartJSON(ctx, client, definitionURL, "publish-"+suffix[:12], definition, &published); err != nil || status != http.StatusCreated {
		first.stop(t)
		t.Fatalf("publish workflow definition: status=%d error=%v body=%s logs=%s", status, err, body, tailAcceptanceLog(redactedCellEngineLogs(first), 8192))
	}
	if published.Data.Digest == "" || published.Data.DefinitionID != definition.ID || published.Data.Version != definition.Version {
		first.stop(t)
		t.Fatalf("published workflow reference = %+v", published.Data)
	}
	instanceURL := fmt.Sprintf("http://127.0.0.1:%d/s/%s/api/v2/workflow-instances", port, entry.SiteID)
	startCommand := contract.StartWorkflow{Meta: contract.CommandMeta{IdempotencyKey: contract.IdempotencyKey("start-" + suffix[:12])},
		DefinitionID: definition.ID, DefinitionVersion: definition.Version,
		Subject: contract.SubjectRef{Kind: "Purchase Request", ID: "PR-" + suffix[:12], Revision: "1"}}
	var started struct {
		Data contract.WorkflowInstance `json:"data"`
	}
	if status, body, err := postWorkflowRestartJSON(ctx, client, instanceURL, string(startCommand.Meta.IdempotencyKey), startCommand, &started); err != nil || status != http.StatusCreated {
		first.stop(t)
		t.Fatalf("start governed approval workflow: status=%d error=%v body=%s logs=%s", status, err, body, tailAcceptanceLog(redactedCellEngineLogs(first), 8192))
	}
	if started.Data.ID == "" {
		first.stop(t)
		t.Fatalf("start workflow returned empty instance: %+v", started.Data)
	}
	waitWorkflowInstanceStatus(t, ctx, tenantDatabase, tenant, started.Data.ID, contract.InstanceWaiting)
	requestID, requestRevision, err := waitWorkflowHumanRequest(t, ctx, tenantDatabase, tenant, started.Data.ID)
	if err != nil {
		first.stop(t)
		t.Fatal("wait for durable Purchase Request assignment:", err)
	}
	var assignmentID string
	if err := tenantDatabase.QueryRowContext(ctx, `SELECT id FROM _kora_request_assignment WHERE tenant_id=? AND request_id=? AND assignee_id=? AND status='pending'`, string(tenant), requestID, managerName).Scan(&assignmentID); err != nil {
		first.stop(t)
		t.Fatal("read assigned Purchase Manager decision:", err)
	}
	managerClient := newWorkflowRestartHTTPClient(t)
	if err := loginWorkflowRestartUser(ctx, managerClient, port, entry.SiteID, managerEmail, password); err != nil {
		first.stop(t)
		t.Fatal("authenticate assigned Purchase Manager:", err)
	}
	responseURL := fmt.Sprintf("http://127.0.0.1:%d/s/%s/api/v2/requests/%s/responses", port, entry.SiteID, requestID)
	responseCommand := contract.RespondRequest{Meta: contract.CommandMeta{IdempotencyKey: contract.IdempotencyKey("approve-" + suffix[:12]), ExpectedRevision: contract.Revision(requestRevision)},
		AssignmentID: contract.AssignmentID(assignmentID), Response: contract.ResponseApprove, Comment: "Acceptance approval"}
	var response struct {
		Data contract.ResponseReceipt `json:"data"`
	}
	decisionDone := make(chan struct {
		status int
		body   string
		err    error
	}, 1)
	go func() {
		status, body, err := postWorkflowRestartJSON(ctx, managerClient, responseURL, string(responseCommand.Meta.IdempotencyKey), responseCommand, &response)
		decisionDone <- struct {
			status int
			body   string
			err    error
		}{status: status, body: body, err: err}
	}()
	select {
	case <-cloudRequestStarted:
	case <-time.After(15 * time.Second):
		releaseCloud.Do(func() { close(cloudRelease) })
		first.stop(t)
		t.Fatal("Engine did not attempt the configured Cloud event delivery during the authorized sign-in")
	case <-ctx.Done():
		releaseCloud.Do(func() { close(cloudRelease) })
		first.stop(t)
		t.Fatal("Engine did not attempt the configured Cloud event delivery during sign-in:", ctx.Err())
	}
	var decisionResult struct {
		status int
		body   string
		err    error
	}
	select {
	case decisionResult = <-decisionDone:
	case <-time.After(5 * time.Second):
		releaseCloud.Do(func() { close(cloudRelease) })
		first.stop(t)
		t.Fatal("authorized tenant decision waited on the stalled Cloud endpoint")
	}
	status, body, err := decisionResult.status, decisionResult.body, decisionResult.err
	if err != nil || status != http.StatusOK {
		releaseCloud.Do(func() { close(cloudRelease) })
		first.stop(t)
		t.Fatalf("authorized Purchase Manager response: status=%d error=%v body=%s logs=%s", status, err, body, tailAcceptanceLog(redactedCellEngineLogs(first), 8192))
	}
	releaseCloud.Do(func() { close(cloudRelease) })
	select {
	case got := <-cloudResponseStatus:
		if got != http.StatusServiceUnavailable {
			first.stop(t)
			t.Fatalf("simulated Cloud outage response = %d, want %d", got, http.StatusServiceUnavailable)
		}
	case <-ctx.Done():
		first.stop(t)
		t.Fatal("stalled Cloud endpoint did not finish its simulated outage response:", ctx.Err())
	}
	if response.Data.Status != contract.RequestResolved || response.Data.Outcome == nil || *response.Data.Outcome != contract.OutcomeApproved {
		first.stop(t)
		t.Fatalf("approval response receipt = %+v", response.Data)
	}
	var requestStatus string
	if err := tenantDatabase.QueryRowContext(ctx, `SELECT status FROM _kora_request WHERE tenant_id=? AND id=?`, string(tenant), requestID).Scan(&requestStatus); err != nil || requestStatus != string(contract.RequestResolved) {
		first.stop(t)
		t.Fatalf("persisted request status=%q err=%v, want resolved", requestStatus, err)
	}
	var domainStatus string
	if err := tenantDatabase.QueryRowContext(ctx, `SELECT status FROM `+dialect.QuoteIdent("tabPurchase Request")+` WHERE name=?`, "PR-"+suffix[:12]).Scan(&domainStatus); err != nil || domainStatus != "Approved" {
		first.stop(t)
		t.Fatalf("approved Purchase Request status=%q err=%v, want Approved", domainStatus, err)
	}
	first.stop(t)
	var workflowStatus string
	if err := tenantDatabase.QueryRowContext(ctx, `SELECT status FROM _kora_workflow_instance WHERE tenant_id=? AND id=?`, string(tenant), string(started.Data.ID)).Scan(&workflowStatus); err != nil || workflowStatus != string(contract.InstanceWaiting) {
		t.Fatalf("workflow before recovery restart=%q err=%v, want waiting after approval receipt", workflowStatus, err)
	}

	second := startCellAcceptanceEngine(t, ctx, root, binary, port, engineEnv)
	second.waitPing(t, ctx, client)
	if got := sitePathPing(client, port, entry.SiteID); got != http.StatusOK {
		second.stop(t)
		t.Fatalf("restarted tenant route = HTTP %d, want 200; logs=%s", got, tailAcceptanceLog(redactedCellEngineLogs(second), 8192))
	}
	waitWorkflowInstanceStatus(t, ctx, tenantDatabase, tenant, started.Data.ID, contract.InstanceCompleted)
	second.stop(t)
	var finalDomainStatus string
	if err := tenantDatabase.QueryRowContext(ctx, `SELECT status FROM `+dialect.QuoteIdent("tabPurchase Request")+` WHERE name=?`, "PR-"+suffix[:12]).Scan(&finalDomainStatus); err != nil || finalDomainStatus != "Approved" {
		t.Fatalf("final Purchase Request status=%q err=%v, want Approved", finalDomainStatus, err)
	}
	t.Logf("Engine restart resumed governed approval workflow %s for tenant %s after authorized decision; Purchase Request reached %s", started.Data.ID, tenant, finalDomainStatus)
}

func seedHumanWorkflowTenant(ctx context.Context, database *sql.DB, dialect db.Dialect, databaseName, siteName, subjectID string) error {
	doctypeDef := &doctype.DocType{Name: "Purchase Request", Module: "Purchasing", Fields: []doctype.Field{
		{Fieldname: "status", Fieldtype: "Data", Default: "Submitted"},
		{Fieldname: "estimated_total", Fieldtype: "Currency"},
	}}
	roles := []*doctype.Role{{Name: "Procurement Officer"}, {Name: "Purchase Manager"}}
	permissions := []*doctype.Permission{
		{Doctype: "Purchase Request", Role: "Procurement Officer", Read: true},
		{Doctype: "Purchase Request", Role: "Purchase Manager", Read: true, Submit: true},
		{Doctype: "Workflow Definition", Role: "Procurement Officer", Read: true, Write: true},
		{Doctype: "Workflow Instance", Role: "Procurement Officer", Read: true, Create: true},
	}
	makeStatusWorkflow := func(id, action, to, condition string, docStatus int) (contract.WorkflowDefinition, error) {
		input, err := json.Marshal(workflow.StatusActionConfig{StateField: "status", InitialState: "Submitted", StatusTransition: workflow.StatusTransition{Action: action, From: "Submitted", To: to, AllowedRoles: []string{"Purchase Manager"}, Condition: condition, DocStatus: docStatus}})
		if err != nil {
			return contract.WorkflowDefinition{}, err
		}
		return contract.WorkflowDefinition{ID: contract.DefinitionID(id), Version: 1, Entry: "transition",
			Trigger: &contract.RecordTrigger{Kind: "record.updated", Table: "Purchase Request", WatchedFields: []string{"status"}},
			Steps: []contract.StepDefinition{
				{ID: "transition", Kind: contract.StepAction, Action: &contract.ActionSpec{Name: "record.update_status", Version: 1, InputSchema: "kora.record.update_status", Input: input, Next: "complete"}},
				{ID: "complete", Kind: contract.StepEnd, End: &contract.EndSpec{Outcome: "complete"}},
			}}, nil
	}
	approveStatusWorkflow, err := makeStatusWorkflow("purchase-request-approve", "Approve", "Approved", "doc.estimated_total > 0", 0)
	if err != nil {
		return err
	}
	rejectStatusWorkflow, err := makeStatusWorkflow("purchase-request-reject", "Reject", "Rejected", "", 1)
	if err != nil {
		return err
	}
	registry := doctype.NewRegistry()
	snapshot := &doctype.ConfigSnapshot{DocTypes: []*doctype.DocType{doctypeDef}, Roles: roles, Permissions: permissions, WorkflowDefinitions: []contract.WorkflowDefinition{approveStatusWorkflow, rejectStatusWorkflow}}
	if err := site.ImportConfigFromSnapshot(database, registry, databaseName, siteName, "workflow restart acceptance", snapshot, dialect, nil); err != nil {
		return err
	}
	_, err = database.ExecContext(ctx, `INSERT INTO `+dialect.QuoteIdent(doctypeDef.RawTableName())+` (`+dialect.QuoteIdent("name")+`,`+dialect.QuoteIdent("owner")+`,`+dialect.QuoteIdent("status")+`,`+dialect.QuoteIdent("estimated_total")+`,`+dialect.QuoteIdent("doc_status")+`,`+dialect.QuoteIdent("revision")+`) VALUES (?,?,?, ?,0,1)`, subjectID, "procurement-seed", "Submitted", 500)
	if err != nil {
		return fmt.Errorf("insert submitted Purchase Request: %w", err)
	}
	return nil
}

func purchaseApprovalRestartDefinition(id string) contract.WorkflowDefinition {
	return contract.WorkflowDefinition{ID: contract.DefinitionID("approval_restart_" + id), Version: 1, Entry: "approval", Steps: []contract.StepDefinition{
		{ID: "approval", Kind: contract.StepHuman, Human: &contract.HumanSpec{Request: contract.HumanRequestTemplate{
			Kind: contract.RequestApproval, Subject: contract.SubjectInstance, Title: "Approve Purchase Request", Message: "Review the submitted Purchase Request.",
			PolicyID: "purchase-request-approval-v1", DueAfterSeconds: 60,
		}, Outcomes: map[contract.ResponseKind]contract.StepID{contract.ResponseApprove: "approved", contract.ResponseReject: "rejected", contract.ResponseReturn: "returned"},
			OnExpired: "expired", OnCancelled: "cancelled", OnStale: "stale"}},
		{ID: "approved", Kind: contract.StepEnd, End: &contract.EndSpec{Outcome: "approved"}},
		{ID: "rejected", Kind: contract.StepEnd, End: &contract.EndSpec{Outcome: "rejected"}},
		{ID: "returned", Kind: contract.StepEnd, End: &contract.EndSpec{Outcome: "returned"}},
		{ID: "expired", Kind: contract.StepEnd, End: &contract.EndSpec{Outcome: "expired"}},
		{ID: "cancelled", Kind: contract.StepEnd, End: &contract.EndSpec{Outcome: "cancelled"}},
		{ID: "stale", Kind: contract.StepEnd, End: &contract.EndSpec{Outcome: "stale"}},
	}}
}

func newWorkflowRestartHTTPClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal("create workflow restart cookie jar:", err)
	}
	return &http.Client{Timeout: 5 * time.Second, Jar: jar}
}

func tailAcceptanceLog(value string, maxBytes int) string {
	if maxBytes <= 0 || len(value) <= maxBytes {
		return value
	}
	return value[len(value)-maxBytes:]
}

func sitePathPingDetails(client *http.Client, port int, siteID string) (int, string) {
	request, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/s/%s/api/v1/ping?workflow_acceptance=1", port, siteID), nil)
	if err != nil {
		return 0, err.Error()
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, err.Error()
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 2048))
	return response.StatusCode, string(body)
}

func loginWorkflowRestartUser(ctx context.Context, client *http.Client, port int, siteID, email, password string) error {
	prefix := fmt.Sprintf("http://127.0.0.1:%d/s/%s", port, siteID)
	response, err := client.Get(prefix + "/api/v1/ping")
	if err != nil {
		return fmt.Errorf("get tenant CSRF cookie: %w", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("get tenant ping returned HTTP %d", response.StatusCode)
	}
	var result map[string]any
	status, body, err := postWorkflowRestartJSON(ctx, client, prefix+"/api/auth/login", "", map[string]string{"email": email, "password": password}, &result)
	if err != nil || status != http.StatusOK {
		return fmt.Errorf("tenant login returned HTTP %d, error=%v body=%s", status, err, body)
	}
	response, err = client.Get(prefix + "/api/v2/requests?view=sent&kind=approval")
	if err != nil {
		return fmt.Errorf("get authenticated request inbox for CSRF cookie: %w", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("authenticated request inbox returned HTTP %d", response.StatusCode)
	}
	csrfPresent := false
	for _, cookie := range client.Jar.Cookies(response.Request.URL) {
		csrfPresent = csrfPresent || cookie.Name == "kora_csrf"
	}
	if !csrfPresent {
		return fmt.Errorf("authenticated tenant GET did not set the CSRF cookie")
	}
	return nil
}

func postWorkflowRestartJSON(ctx context.Context, client *http.Client, target, idempotency string, input, output any) (int, string, error) {
	encoded, err := json.Marshal(input)
	if err != nil {
		return 0, "", err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(encoded))
	if err != nil {
		return 0, "", err
	}
	request.Header.Set("Content-Type", "application/json")
	if idempotency != "" {
		request.Header.Set("Idempotency-Key", idempotency)
	}
	for _, cookie := range client.Jar.Cookies(request.URL) {
		if cookie.Name == "kora_csrf" {
			request.Header.Set("X-Kora-CSRF-Token", cookie.Value)
			break
		}
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, "", err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return response.StatusCode, "", err
	}
	if output != nil && len(body) > 0 {
		if err := json.Unmarshal(body, output); err != nil {
			return response.StatusCode, string(body), fmt.Errorf("decode response JSON: %w", err)
		}
	}
	return response.StatusCode, string(body), nil
}

func waitWorkflowHumanRequest(t *testing.T, ctx context.Context, database *sql.DB, tenant contract.TenantID, instance contract.InstanceID) (string, string, error) {
	t.Helper()
	deadline, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	for {
		var id string
		var revision int64
		err := database.QueryRowContext(deadline, `SELECT id,revision FROM _kora_request WHERE tenant_id=? AND source_instance_id=? AND source_step_id='approval' ORDER BY created_at DESC LIMIT 1`, string(tenant), string(instance)).Scan(&id, &revision)
		if err == nil {
			return id, strconv.FormatInt(revision, 10), nil
		}
		if err != sql.ErrNoRows {
			return "", "", err
		}
		select {
		case <-deadline.Done():
			return "", "", fmt.Errorf("workflow request did not appear before timeout: %w", deadline.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func isolatedWorkflowRestartEnvironment(dsn, platformPort, configDir, consumerID string, enableScripts ...bool) []string {
	allowed := map[string]bool{"PATH": true, "HOME": true, "TMPDIR": true, "LANG": true, "LC_ALL": true}
	filtered := make([]string, 0, len(allowed)+12)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if allowed[key] {
			filtered = append(filtered, entry)
		}
	}
	return append(filtered,
		"DB_DSN="+dsn,
		"KORA_DB_TYPE=mysql",
		"KORA_DB_HOST=127.0.0.1",
		"KORA_DB_PORT="+platformPort,
		"KORA_DB_USER=root",
		"KORA_DB_PASSWORD=acceptance-only",
		"KORA_CONFIG_DIR="+configDir,
		"KORA_SITE_DIRECTORY_CONSUMER_ID="+consumerID,
		"KORA_DB_MAX_OPEN=2",
		"KORA_DB_MAX_IDLE=1",
		"KORA_SCRIPTS_ENABLED="+strconv.FormatBool(len(enableScripts) > 0 && enableScripts[0]),
		"KORA_LOG_LEVEL=debug",
		"KORA_LOG_FORMAT=json",
	)
}

func waitDurableLifecycleHookStatus(t *testing.T, ctx context.Context, database *sql.DB, tenant contract.TenantID, source contract.OperationID, want string) {
	t.Helper()
	deadline, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	var last string
	for {
		err := database.QueryRowContext(deadline, "SELECT status FROM _kora_script_invocation WHERE tenant_id = ? AND source_operation_id = ? AND sequence_no = 1", string(tenant), string(source)).Scan(&last)
		if err == nil && last == want {
			return
		}
		select {
		case <-deadline.Done():
			t.Fatalf("durable lifecycle hook %s status = %q, want %q (last query error: %v)", source, last, want, err)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func waitWorkflowInstanceStatus(t *testing.T, ctx context.Context, database *sql.DB, tenant contract.TenantID, id contract.InstanceID, want contract.InstanceStatus) {
	t.Helper()
	deadline, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	var last string
	for {
		var status string
		err := database.QueryRowContext(deadline, "SELECT status FROM _kora_workflow_instance WHERE tenant_id = ? AND id = ?", string(tenant), string(id)).Scan(&status)
		if err == nil {
			last = status
			if contract.InstanceStatus(status) == want {
				return
			}
		}
		select {
		case <-deadline.Done():
			t.Fatalf("workflow instance %s status = %q, want %q (last query error: %v)", id, last, want, err)
		case <-time.After(100 * time.Millisecond):
		}
	}
}
