package kernel

import (
	"context"
	"encoding/json"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/orm"
	"github.com/asenawritescode/kora/policy"
	"github.com/asenawritescode/kora/script"
)

func TestVerifiedExecutionOverridesCallerActorAndRoles(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO _kora_operation_audit").WithArgs(
		sqlmock.AnyArg(), "tenant-a", sqlmock.AnyArg(), "", "", "", "human", "creator", "creator", "",
		"record.create", "Task", "", "rejected", string(contract.CodePermissionDenied), sqlmock.AnyArg(), "", "",
	).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	registry := doctype.NewRegistry()
	registry.LoadFull([]*doctype.DocType{{Name: "Task", Fields: []doctype.Field{{Fieldname: "title", Fieldtype: "Data"}}}},
		[]*doctype.Role{{Name: "Administrator"}, {Name: "Creator"}},
		[]*doctype.Permission{{Doctype: "Task", Role: "Administrator", Create: true}})
	execution, err := policy.NewVerifiedExecution("stable-id-42", contract.ActorContext{
		PrincipalID: "creator", PrincipalType: contract.PrincipalHuman, Site: "tenant-a", Roles: []string{"Creator"}, AuthenticatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]any{"title": "test"})
	payload, _ := json.Marshal(RecordCreatePayload{Doctype: "Task", Data: data})
	op := Operation{Command: CommandRecordCreate, Payload: payload}
	op.Context = OperationContext{
		Site: "tenant-a", TenantID: "stable-id-42", Execution: execution, Actor: contract.ActorContext{PrincipalID: "admin", PrincipalType: contract.PrincipalHuman, Site: "tenant-a", Roles: []string{"Administrator"}},
		User: "admin", Roles: []string{"Administrator"},
	}
	result, operationErr := New(db.Resolve("mysql"), nil).Execute(context.Background(), database, registry, op)
	if operationErr == nil || operationErr.Type != contract.CodePermissionDenied || result.Status != contract.StatusRejected {
		t.Fatalf("result=%#v error=%#v; verified non-admin actor should be denied", result, operationErr)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestVerifiedExecutionRejectsMismatchedOperationTenantBeforeDatabase(t *testing.T) {
	execution, err := policy.NewVerifiedExecution("tenant-a", contract.ActorContext{PrincipalID: "user", PrincipalType: contract.PrincipalHuman, Site: "tenant-a"})
	if err != nil {
		t.Fatal(err)
	}
	op := Operation{Command: CommandRecordCreate}
	op.Context = OperationContext{Site: "tenant-b", Execution: execution}
	_, operationErr := New(db.Resolve("mysql"), nil).Execute(context.Background(), nil, nil, op)
	if operationErr == nil || operationErr.Type != contract.CodePermissionDenied {
		t.Fatalf("mismatched tenant error=%#v, want permission denied", operationErr)
	}
}

func TestExecuteInTxRequiresVerifiedExecution(t *testing.T) {
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
	_, _, commandErr := New(db.Resolve("mysql"), nil).ExecuteInTx(context.Background(), database, tx, doctype.NewRegistry(), Operation{
		Command: CommandRecordCreate,
		Context: OperationContext{Site: "tenant-a", Actor: contract.ActorContext{PrincipalID: "caller", PrincipalType: contract.PrincipalHuman, Site: "tenant-a", AuthenticatedAt: time.Now()}},
	})
	if commandErr == nil || commandErr.Type != contract.CodeUnauthenticated {
		_ = tx.Rollback()
		t.Fatalf("unverified transaction-bound execution error = %#v, want unauthenticated", commandErr)
	}
	mock.ExpectRollback()
	if err := tx.Rollback(); err != nil {
		t.Fatal("rollback caller transaction:", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestBindScriptProviderUsesOperationScopedTransactionManager(t *testing.T) {
	var received *orm.TxManager
	tx := &orm.TxManager{
		SiteName: "tenant-a",
		ScriptProviderFactory: func(bound *orm.TxManager) script.KoraProvider {
			received = bound
			return script.NoopProvider{}
		},
	}
	bindScriptProvider(tx)
	if received != tx {
		t.Fatal("provider factory did not receive the operation-scoped transaction manager")
	}
	if tx.ScriptProvider == nil {
		t.Fatal("provider factory result was not installed on the operation transaction")
	}
}

func TestScopedReceiptKeyUsesVerifiedTenantPrincipalAndCommand(t *testing.T) {
	base, err := policy.NewVerifiedExecution("stable-tenant-a", contract.ActorContext{
		PrincipalID: "user-a", PrincipalType: contract.PrincipalHuman, Site: "tenant-route-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	op := Operation{Command: CommandRecordCreate, Context: OperationContext{
		TenantID: base.Tenant(), Site: "tenant-route-a", Execution: base, IdempotencyKey: "client-key",
	}}
	want := scopedReceiptKey(op)
	if want == "" || want[:len("scope-v2:")] != "scope-v2:" || len(want) != len("scope-v2:")+64 {
		t.Fatalf("scoped receipt key = %q, want versioned SHA-256 key", want)
	}
	if got := scopedReceiptKey(op); got != want {
		t.Fatalf("same verified identity produced unstable receipt key: %q != %q", got, want)
	}

	otherPrincipal, err := policy.NewVerifiedExecution("stable-tenant-a", contract.ActorContext{
		PrincipalID: "user-b", PrincipalType: contract.PrincipalHuman, Site: "tenant-route-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	other := op
	other.Context.Execution = otherPrincipal
	if scopedReceiptKey(other) == want {
		t.Fatal("different verified principal shared an idempotency scope")
	}
	other = op
	other.Command = CommandRecordMutateBundle
	if scopedReceiptKey(other) == want {
		t.Fatal("different command shared an idempotency scope")
	}
	other = op
	other.Context.IdempotencyKey = "another-key"
	if scopedReceiptKey(other) == want {
		t.Fatal("different caller key shared an idempotency scope")
	}
	other = op
	other.Context.Execution, err = policy.NewVerifiedExecution("stable-tenant-b", contract.ActorContext{
		PrincipalID: "user-a", PrincipalType: contract.PrincipalHuman, Site: "tenant-route-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	if scopedReceiptKey(other) == want {
		t.Fatal("different stable tenant shared an idempotency scope")
	}
}

func TestScopedReceiptStorageUsesStableTenantAcrossRouteRename(t *testing.T) {
	makeExecution := func(route string) policy.Execution {
		execution, err := policy.NewVerifiedExecution("stable-tenant-a", contract.ActorContext{
			PrincipalID: "user-a", PrincipalType: contract.PrincipalHuman, Site: route,
		})
		if err != nil {
			t.Fatal("create verified execution:", err)
		}
		return execution
	}
	first := Operation{Command: CommandRecordCreate, Context: OperationContext{
		Site: "shop.old.example", Execution: makeExecution("shop.old.example"),
	}}
	second := Operation{Command: CommandRecordCreate, Context: OperationContext{
		Site: "shop.example", Execution: makeExecution("shop.example"),
	}}
	if got := receiptStorageTenant(first); got != "stable-tenant-a" {
		t.Fatalf("first receipt storage tenant = %q", got)
	}
	if got := receiptStorageTenant(second); got != "stable-tenant-a" {
		t.Fatalf("renamed receipt storage tenant = %q", got)
	}
	if scopedReceiptKey(first) != scopedReceiptKey(second) {
		t.Fatal("route rename changed the scoped receipt key")
	}
}

func TestLookupScopedReceiptFindsPriorRouteAndRejectsDuplicateRows(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	execution, err := policy.NewVerifiedExecution("stable-tenant", contract.ActorContext{
		PrincipalID: "alice", PrincipalType: contract.PrincipalHuman, Site: "tenant.new-route",
	})
	if err != nil {
		t.Fatal(err)
	}
	prior := contract.CommandResult{OperationID: "prior-operation", Status: contract.StatusCompleted, Data: json.RawMessage(`{"created":true}`)}
	resultJSON, err := json.Marshal(prior)
	if err != nil {
		t.Fatal(err)
	}
	op := Operation{Command: CommandRecordCreate, Payload: json.RawMessage(`{"doctype":"Task"}`), Context: OperationContext{
		Site: "tenant.new-route", User: "alice", Execution: execution, IdempotencyKey: "retry-key",
	}}
	lookup := regexp.QuoteMeta("SELECT operation_id, command_name, actor_user, payload_hash, result_hash, status, result_json FROM _kora_idempotency_receipt\n\t\t WHERE idempotency_key = ? LIMIT 2")
	columns := []string{"operation_id", "command_name", "actor_user", "payload_hash", "result_hash", "status", "result_json"}
	mock.ExpectQuery(lookup).WithArgs(scopedReceiptKey(op)).WillReturnRows(sqlmock.NewRows(columns).
		AddRow("prior-operation", CommandRecordCreate, "alice", payloadHash(op.Payload), resultHash(prior), receiptCompleted, string(resultJSON)))
	result, found, commandErr := New(db.Resolve("mysql"), nil).lookupReceipt(context.Background(), database, op)
	if commandErr != nil || !found || !result.Replayed || result.OperationID != prior.OperationID {
		t.Fatalf("route-renamed scoped receipt = (%#v, found=%v, error=%v)", result, found, commandErr)
	}

	duplicateDB, duplicateMock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer duplicateDB.Close()
	duplicateMock.ExpectQuery(lookup).WithArgs(scopedReceiptKey(op)).WillReturnRows(sqlmock.NewRows(columns).
		AddRow("first-operation", CommandRecordCreate, "alice", payloadHash(op.Payload), resultHash(prior), receiptCompleted, string(resultJSON)).
		AddRow("second-operation", CommandRecordCreate, "alice", payloadHash(op.Payload), resultHash(prior), receiptCompleted, string(resultJSON)))
	_, found, commandErr = New(db.Resolve("mysql"), nil).lookupReceipt(context.Background(), duplicateDB, op)
	if !found || commandErr == nil || commandErr.Type != contract.CodeConflict {
		t.Fatalf("ambiguous receipt lookup = (found=%v, error=%v), want conflict", found, commandErr)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("unmet route-rename expectations:", err)
	}
	if err := duplicateMock.ExpectationsWereMet(); err != nil {
		t.Fatal("unmet ambiguous-receipt expectations:", err)
	}
}

func TestPayloadHashCanonicalizesJSONWithoutRoundingIntegers(t *testing.T) {
	first := json.RawMessage(`{"b":2,"large":9007199254740993,"a":1}`)
	second := json.RawMessage("{ \"a\": 1, \"large\": 9007199254740993, \"b\": 2 }\n")
	if payloadHash(first) != payloadHash(second) {
		t.Fatal("equivalent JSON objects did not produce the same canonical payload hash")
	}
	changed := json.RawMessage(`{"a":1,"large":9007199254740992,"b":2}`)
	if payloadHash(first) == payloadHash(changed) {
		t.Fatal("distinct large integer values produced the same canonical payload hash")
	}
	if rawPayloadHash(first) == payloadHash(first) {
		t.Fatal("test requires canonical and legacy raw-byte hashes to use separate formats")
	}
}

func TestLegacyReceiptReplayIsLimitedToMatchingDirectHuman(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	actor := contract.ActorContext{PrincipalID: "alice", PrincipalType: contract.PrincipalHuman, Site: "tenant-route"}
	execution, err := policy.NewVerifiedExecution("stable-tenant", actor)
	if err != nil {
		t.Fatal(err)
	}
	op := Operation{Command: CommandRecordCreate, Payload: json.RawMessage(`{"doctype":"Task"}`), Context: OperationContext{
		Site: "tenant-route", User: "alice", Execution: execution, IdempotencyKey: "old-client-key",
	}}
	prior := contract.CommandResult{OperationID: "old-operation", Status: contract.StatusCompleted, Data: json.RawMessage(`{"ok":true}`)}
	resultJSON, err := json.Marshal(prior)
	if err != nil {
		t.Fatal(err)
	}
	query := regexp.QuoteMeta("SELECT operation_id, command_name, actor_user, payload_hash, result_hash, status, result_json FROM _kora_idempotency_receipt\n\t\t WHERE idempotency_key = ? LIMIT 2")
	mock.ExpectQuery(query).WithArgs(scopedReceiptKey(op)).WillReturnRows(
		sqlmock.NewRows([]string{"operation_id", "command_name", "actor_user", "payload_hash", "result_hash", "status", "result_json"}),
	)
	legacyQuery := regexp.QuoteMeta("SELECT operation_id, command_name, actor_user, payload_hash, result_hash, status, result_json\n\t\tFROM _kora_idempotency_receipt WHERE idempotency_key = ? AND command_name = ? AND actor_user = ? ORDER BY created_at DESC LIMIT 2")
	mock.ExpectQuery(legacyQuery).WithArgs("old-client-key", CommandRecordCreate, "alice").WillReturnRows(sqlmock.NewRows([]string{
		"operation_id", "command_name", "actor_user", "payload_hash", "result_hash", "status", "result_json",
	}).AddRow("old-operation", CommandRecordCreate, "alice", payloadHash(op.Payload), resultHash(prior), receiptCompleted, string(resultJSON)))

	result, found, commandErr := New(db.Resolve("mysql"), nil).lookupReceipt(context.Background(), database, op)
	if commandErr != nil || !found || !result.Replayed || result.OperationID != prior.OperationID || string(result.Data) != string(prior.Data) {
		t.Fatalf("matching direct-human legacy receipt = (%#v, found=%v, error=%v)", result, found, commandErr)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}

	serviceActor := contract.ActorContext{PrincipalID: "extension:one", PrincipalType: contract.PrincipalService, SubjectUserID: "alice", Site: "tenant-route", DelegationID: "grant-1"}
	serviceExecution, err := policy.NewVerifiedExecution("stable-tenant", serviceActor)
	if err != nil {
		t.Fatal(err)
	}
	serviceOp := op
	serviceOp.Context.Execution = serviceExecution
	mock.ExpectQuery(query).WithArgs(scopedReceiptKey(serviceOp)).WillReturnRows(
		sqlmock.NewRows([]string{"operation_id", "command_name", "actor_user", "payload_hash", "result_hash", "status", "result_json"}),
	)
	result, found, commandErr = New(db.Resolve("mysql"), nil).lookupReceipt(context.Background(), database, serviceOp)
	if commandErr != nil || found || result.Replayed {
		t.Fatalf("service execution reused legacy human receipt: (%#v, found=%v, error=%v)", result, found, commandErr)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestUnverifiedCallerRolesCannotAuthorizeKernelCommand(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("create SQL mock:", err)
	}
	defer database.Close()
	registry := doctype.NewRegistry()
	registry.LoadFull(
		[]*doctype.DocType{{Name: "Task", Fields: []doctype.Field{{Fieldname: "title", Fieldtype: "Data"}}}},
		[]*doctype.Role{{Name: doctype.AdminRole}},
		[]*doctype.Permission{{Doctype: "Task", Role: doctype.AdminRole, Create: true}},
	)
	payload, err := json.Marshal(RecordCreatePayload{Doctype: "Task", Data: json.RawMessage(`{"title":"must not be created"}`)})
	if err != nil {
		t.Fatal("encode create payload:", err)
	}
	result, commandErr := New(db.Resolve("mysql"), nil).Execute(context.Background(), database, registry, Operation{
		Command: CommandRecordCreate,
		Payload: payload,
		Context: OperationContext{Site: "tenant-a", Roles: []string{doctype.AdminRole}, Actor: contract.ActorContext{
			PrincipalID: "caller", PrincipalType: contract.PrincipalHuman, Site: "tenant-a", Roles: []string{doctype.AdminRole}, AuthenticatedAt: time.Now().UTC(),
		}},
	})
	if commandErr == nil || commandErr.Type != contract.CodeUnauthenticated || result.Status != contract.StatusRejected {
		t.Fatalf("caller-supplied Administrator execution = (%#v, %v), want unauthenticated", result, commandErr)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unverified execution touched database: %v", err)
	}
}

func TestVerifiedExecutionWithNoRolesDoesNotInheritAdministrator(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO _kora_operation_audit").WithArgs(
		sqlmock.AnyArg(), "tenant-a", sqlmock.AnyArg(), "", "", "", "human", "roleless", "roleless", "",
		"record.create", "Task", "", "rejected", string(contract.CodePermissionDenied), sqlmock.AnyArg(), "", "",
	).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	registry := doctype.NewRegistry()
	registry.LoadFull([]*doctype.DocType{{Name: "Task", Fields: []doctype.Field{{Fieldname: "title", Fieldtype: "Data"}}}},
		[]*doctype.Role{{Name: doctype.AdminRole}},
		[]*doctype.Permission{{Doctype: "Task", Role: doctype.AdminRole, Create: true}})
	execution, err := policy.NewVerifiedExecution("stable-id-42", contract.ActorContext{
		PrincipalID: "roleless", PrincipalType: contract.PrincipalHuman, Site: "tenant-a", AuthenticatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]any{"title": "test"})
	payload, _ := json.Marshal(RecordCreatePayload{Doctype: "Task", Data: data})
	result, operationErr := New(db.Resolve("mysql"), nil).Execute(context.Background(), database, registry, Operation{
		Command: CommandRecordCreate, Payload: payload,
		Context: OperationContext{Site: "tenant-a", TenantID: "stable-id-42", Execution: execution},
	})
	if operationErr == nil || operationErr.Type != contract.CodePermissionDenied || result.Status != contract.StatusRejected {
		t.Fatalf("result=%#v error=%#v; verified actor without roles must not inherit Administrator", result, operationErr)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestVerifiedExecutionWithNoRolesCannotAuthorizeMutationBundle(t *testing.T) {
	execution, err := policy.NewVerifiedExecution("tenant-a", contract.ActorContext{
		PrincipalID: "roleless", PrincipalType: contract.PrincipalHuman, Site: "tenant-a", AuthenticatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	registry := doctype.NewRegistry()
	registry.LoadFull([]*doctype.DocType{{Name: "Task"}}, []*doctype.Role{{Name: doctype.AdminRole}},
		[]*doctype.Permission{{Doctype: "Task", Role: doctype.AdminRole, Create: true}})
	payload, err := json.Marshal(RecordMutationBundlePayload{Doctype: "Task", Records: []RecordMutationBundleItem{{Key: "task", Doctype: "Task", Data: json.RawMessage(`{"title":"test"}`)}}})
	if err != nil {
		t.Fatal(err)
	}
	op := Operation{Command: CommandRecordMutateBundle, Payload: payload}
	op.Context = OperationContext{Site: "tenant-a", TenantID: "tenant-a", Execution: execution}
	definition, _ := LookupCommand(CommandRecordMutateBundle)
	if got := authorizeOp(registry, op, definition, nil); got == nil || got.Type != contract.CodePermissionDenied {
		t.Fatalf("verified roleless bundle authorization = %v, want permission denied", got)
	}
}

func TestVerifiedDelegatedGrantAuthorizesOnlyItsExactOperation(t *testing.T) {
	execution, err := policy.NewVerifiedExecutionWithGrants("tenant-a", contract.ActorContext{
		PrincipalID: "extension:reporter", PrincipalType: contract.PrincipalService, Site: "tenant-a", AuthenticatedAt: time.Now(),
	}, []policy.Grant{{Resource: "Task", Operation: "create"}})
	if err != nil {
		t.Fatal(err)
	}
	registry := doctype.NewRegistry()
	registry.Register(&doctype.DocType{Name: "Task"})
	op := Operation{Command: CommandRecordCreate}
	op.Context = OperationContext{Site: "tenant-a", TenantID: "tenant-a", Execution: execution}
	create := RecordCreatePayload{Doctype: "Task", Data: json.RawMessage(`{"title":"test"}`)}
	op.Payload, err = json.Marshal(create)
	if err != nil {
		t.Fatal(err)
	}
	definition, _ := LookupCommand(CommandRecordCreate)
	if got := authorizeOp(registry, op, definition, nil); got != nil {
		t.Fatalf("exact verified create grant denied: %v", got)
	}
	definition.AuthorizesWith = "write"
	if got := authorizeOp(registry, op, definition, nil); got == nil || got.Type != contract.CodePermissionDenied {
		t.Fatalf("create grant authorized write: %v", got)
	}
}

func TestRolePermissionAuthorizesResourceAlias(t *testing.T) {
	registry := doctype.NewRegistry()
	registry.LoadFull([]*doctype.DocType{{Name: "Lead"}}, []*doctype.Role{{Name: "Sales Rep"}}, []*doctype.Permission{
		{Doctype: "Lead", Role: "Sales Rep", Create: true, Read: true, IfOwner: true},
	})
	execution, err := policy.NewVerifiedExecution("tenant-a", contract.ActorContext{
		PrincipalID: "rep-1", SubjectUserID: "rep-1", PrincipalType: contract.PrincipalHuman,
		Site: "tenant-a", Roles: []string{"Sales Rep"}, AuthenticatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(RecordCreatePayload{Doctype: "lead", Data: json.RawMessage(`{"lead_name":"Prospect"}`)})
	if err != nil {
		t.Fatal(err)
	}
	definition, _ := LookupCommand(CommandRecordCreate)
	if got := authorizeOp(registry, Operation{Command: CommandRecordCreate, Payload: payload, Context: OperationContext{Execution: execution}}, definition, nil); got != nil {
		t.Fatalf("Sales Rep create permission denied for resource alias: %v", got)
	}
}
