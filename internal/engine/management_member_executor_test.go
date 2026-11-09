package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/api"
	"github.com/asenawritescode/kora/internal/auth"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/management"
	knet "github.com/asenawritescode/kora/internal/net"
	"github.com/asenawritescode/kora/internal/site"
	_ "github.com/mattn/go-sqlite3"
)

const grantTestSite = "shop.example.test"

type memberTestEnv struct {
	database *sql.DB
	executor *managementMemberExecutor
	released int
}

var (
	memberTemplateOnce sync.Once
	memberTemplatePath string
	memberTemplateErr  error
)

// memberTemplateDB bootstraps the tenant system tables once; bootstrapping is
// slow, so every test copies the resulting file.
func memberTemplateDB(t *testing.T) []byte {
	t.Helper()
	memberTemplateOnce.Do(func() {
		dir, err := os.MkdirTemp("", "member-template")
		if err != nil {
			memberTemplateErr = err
			return
		}
		memberTemplatePath = filepath.Join(dir, "template.db")
		database, err := sql.Open("sqlite3", memberTemplatePath)
		if err != nil {
			memberTemplateErr = err
			return
		}
		defer database.Close()
		memberTemplateErr = site.BootstrapSystemTables(context.Background(), database, db.Resolve("libsql"))
	})
	if memberTemplateErr != nil {
		t.Fatal(memberTemplateErr)
	}
	data, err := os.ReadFile(memberTemplatePath)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// memberTestDatabase opens a fresh bootstrapped tenant database. The default is
// SQLite on the LibSQL dialect; the integration build swaps in MySQL.
var memberTestDatabase = openSQLiteMemberDatabase

func openSQLiteMemberDatabase(t *testing.T) (*sql.DB, db.Dialect) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tenant.db")
	if err := os.WriteFile(path, memberTemplateDB(t), 0o600); err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database, db.Resolve("libsql")
}

func newMemberTestEnv(t *testing.T) *memberTestEnv {
	t.Helper()
	database, dialect := memberTestDatabase(t)
	return newMemberTestEnvOn(t, database, dialect)
}

// newMemberTestEnvOn seeds roles into an already bootstrapped tenant database.
func newMemberTestEnvOn(t *testing.T, database *sql.DB, dialect db.Dialect) *memberTestEnv {
	t.Helper()
	var err error
	// The administrator role name is process-wide configuration other tests change.
	previousAdmin := doctype.AdminRole
	doctype.SetAdminRole("Administrator")
	t.Cleanup(func() { doctype.SetAdminRole(previousAdmin) })
	env := &memberTestEnv{database: database}
	for _, role := range []string{"Cashier", "Stock Clerk"} {
		if _, err := database.Exec(db.Rebind(dialect, `INSERT INTO _kora_role (name, site) VALUES (?, '')`), role); err != nil {
			t.Fatal(err)
		}
	}
	// A role scoped to another site must not be assignable here.
	if _, err := database.Exec(`INSERT INTO _kora_role (name, site) VALUES ('Other Shop Role', 'other.example.test')`); err != nil {
		t.Fatal(err)
	}
	env.executor, err = newManagementMemberExecutor(func(_ context.Context, tenant contract.TenantID) (managementMemberRuntime, func(), error) {
		if tenant != "tenant-a" {
			return managementMemberRuntime{}, nil, errors.New("tenant is not active")
		}
		return managementMemberRuntime{DB: database, Dialect: dialect, Site: grantTestSite}, func() { env.released++ }, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func (e *memberTestEnv) insertUser(t *testing.T, name, email, passwordHash string, enabled bool, roles string) {
	t.Helper()
	if _, err := e.database.Exec(`INSERT INTO _kora_user (name, site, email, password_hash, full_name, enabled, roles) VALUES (?, ?, ?, ?, 'Existing', ?, ?)`, name, grantTestSite, email, passwordHash, enabled, roles); err != nil {
		t.Fatal(err)
	}
}

func (e *memberTestEnv) membershipRevision(t *testing.T) int {
	t.Helper()
	var revision int
	if err := e.database.QueryRow(`SELECT membership_revision FROM _kora_auth_revision WHERE id = 1`).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	return revision
}

func (e *memberTestEnv) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := e.database.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func memberGrant() contract.GrantTenantMember {
	return contract.GrantTenantMember{
		Meta: contract.CommandMeta{IdempotencyKey: "inv_01K7INVITATION"}, InvitationID: "inv_01K7INVITATION",
		Subject: "usr_member_1", Email: "cashier@example.test", FullName: "Amina Otieno", Roles: []string{"Cashier"},
	}
}

func memberClaim(t *testing.T, command contract.GrantTenantMember) management.Claim {
	t.Helper()
	payload, err := json.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}
	return management.Claim{Owner: "worker-1", Fence: 1, Operation: management.Operation{
		ID: "01K7OPERATION0000000000000", Tenant: "tenant-a", Caller: "cloud-control-plane",
		IdempotencyKey: command.Meta.IdempotencyKey, Kind: management.CommandTenantMemberGrant, Command: payload,
	}}
}

func TestManagementMemberGrantCreatesTheInvitedRoleOnly(t *testing.T) {
	env := newMemberTestEnv(t)
	before := env.membershipRevision(t)
	result, err := env.executor.Execute(context.Background(), memberClaim(t, memberGrant()))
	if err != nil || result.State != contract.OperationSucceeded || result.Error != nil {
		t.Fatalf("grant result=%+v err=%v", result, err)
	}
	var name, email, fullName, hash, roles string
	var enabled bool
	var verified sql.NullString
	if err := env.database.QueryRow(`SELECT name, email, full_name, password_hash, enabled, email_verified_at, roles FROM _kora_user WHERE site = ? AND name = ?`, grantTestSite, "usr_member_1").
		Scan(&name, &email, &fullName, &hash, &enabled, &verified, &roles); err != nil {
		t.Fatal(err)
	}
	if email != "cashier@example.test" || fullName != "Amina Otieno" || hash != site.PasswordlessPasswordHash || !enabled || !verified.Valid || roles != "Cashier" {
		t.Fatalf("row email=%q name=%q hash=%q enabled=%v verified=%v roles=%q", email, fullName, hash, enabled, verified, roles)
	}
	user, err := auth.NewOwnedSessionManagerPaused(env.database).FindUserByCloudSubject(grantTestSite, "usr_member_1")
	if err != nil || len(user.Roles) != 1 || user.Roles[0] != "Cashier" {
		t.Fatalf("handoff lookup user=%+v err=%v", user, err)
	}
	if after := env.membershipRevision(t); after != before+1 {
		t.Fatalf("membership revision %d -> %d, want one bump", before, after)
	}
	var principal, correlation, command string
	if err := env.database.QueryRow(`SELECT principal_id, correlation_id, command_name FROM _kora_operation_audit WHERE operation_id = ?`, "01K7OPERATION0000000000000").Scan(&principal, &correlation, &command); err != nil {
		t.Fatalf("audit row: %v", err)
	}
	if principal != "cloud-control-plane" || correlation != "inv_01K7INVITATION" || command != "tenant_member_grant" {
		t.Fatalf("audit principal=%q invitation=%q command=%q", principal, correlation, command)
	}
	if env.released != 1 {
		t.Fatalf("tenant runtime released %d times", env.released)
	}
}

func TestManagementMemberGrantIsIdempotentPerInvitation(t *testing.T) {
	env := newMemberTestEnv(t)
	claim := memberClaim(t, memberGrant())
	if result, err := env.executor.Execute(context.Background(), claim); err != nil || result.State != contract.OperationSucceeded {
		t.Fatalf("first grant result=%+v err=%v", result, err)
	}
	after := env.membershipRevision(t)
	if result, err := env.executor.Execute(context.Background(), claim); err != nil || result.State != contract.OperationSucceeded {
		t.Fatalf("replayed grant result=%+v err=%v", result, err)
	}
	if got := env.count(t, `SELECT COUNT(*) FROM _kora_user WHERE site = ? AND name = ?`, grantTestSite, "usr_member_1"); got != 1 {
		t.Fatalf("%d member rows after replay", got)
	}
	if got := env.count(t, `SELECT COUNT(*) FROM _kora_operation_audit WHERE doc_name = ?`, "usr_member_1"); got != 1 {
		t.Fatalf("%d audit rows after replay", got)
	}
	if got := env.membershipRevision(t); got != after {
		t.Fatalf("replay bumped membership revision %d -> %d", after, got)
	}
}

func TestManagementMemberGrantRefusals(t *testing.T) {
	tests := []struct {
		name     string
		seed     func(*testing.T, *memberTestEnv)
		mutate   func(*contract.GrantTenantMember)
		wantCode contract.Code
		wantText string
	}{
		{name: "administrator role", mutate: func(c *contract.GrantTenantMember) { c.Roles = []string{"Administrator"} }, wantCode: contract.CodeValidationFailed, wantText: "role.not_assignable"},
		{name: "administrator role in another case", mutate: func(c *contract.GrantTenantMember) { c.Roles = []string{"administrator"} }, wantCode: contract.CodeValidationFailed, wantText: "role.not_assignable"},
		{name: "unknown role", mutate: func(c *contract.GrantTenantMember) { c.Roles = []string{"Auditor"} }, wantCode: contract.CodeValidationFailed, wantText: "role.unknown"},
		{name: "role of another site", mutate: func(c *contract.GrantTenantMember) { c.Roles = []string{"Other Shop Role"} }, wantCode: contract.CodeValidationFailed, wantText: "role.unknown"},
		{name: "role differing only in case", mutate: func(c *contract.GrantTenantMember) { c.Roles = []string{"cashier"} }, wantCode: contract.CodeValidationFailed, wantText: "role.unknown"},
		{
			name: "email of a local password user", wantCode: contract.CodeConflict, wantText: "member.email_in_use",
			seed: func(t *testing.T, e *memberTestEnv) {
				e.insertUser(t, "local-user", "Cashier@Example.test", "$2a$10$hash", true, "Cashier")
			},
		},
		{
			name: "email of another cloud member", wantCode: contract.CodeConflict, wantText: "member.email_in_use",
			seed: func(t *testing.T, e *memberTestEnv) {
				e.insertUser(t, "usr_other", "cashier@example.test", site.PasswordlessPasswordHash, true, "Cashier")
			},
		},
		{
			name: "subject already bound to another email", wantCode: contract.CodeConflict, wantText: "member.already_exists",
			seed: func(t *testing.T, e *memberTestEnv) {
				e.insertUser(t, "usr_member_1", "someone.else@example.test", site.PasswordlessPasswordHash, true, "Cashier")
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			env := newMemberTestEnv(t)
			if test.seed != nil {
				test.seed(t, env)
			}
			command := memberGrant()
			if test.mutate != nil {
				test.mutate(&command)
			}
			rowsBefore := env.count(t, `SELECT COUNT(*) FROM _kora_user`)
			revision := env.membershipRevision(t)
			result, err := env.executor.Execute(context.Background(), memberClaim(t, command))
			if err != nil || result.State != contract.OperationFailed || result.Error == nil || result.Error.Type != test.wantCode || !strings.HasPrefix(result.Error.Message, test.wantText+":") {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if got := env.count(t, `SELECT COUNT(*) FROM _kora_user`); got != rowsBefore {
				t.Fatalf("refused grant changed user rows %d -> %d", rowsBefore, got)
			}
			if got := env.membershipRevision(t); got != revision {
				t.Fatalf("refused grant bumped the membership revision %d -> %d", revision, got)
			}
			if got := env.count(t, `SELECT COUNT(*) FROM _kora_operation_audit`); got != 0 {
				t.Fatalf("refused grant wrote %d audit rows", got)
			}
		})
	}
}

func TestManagementMemberGrantReusesTheMemberRowOfTheSameSubject(t *testing.T) {
	t.Run("revoked member is re-enabled with the new role", func(t *testing.T) {
		env := newMemberTestEnv(t)
		env.insertUser(t, "usr_member_1", "Cashier@example.test", site.PasswordlessPasswordHash, false, "")
		command := memberGrant()
		command.Roles = []string{"Stock Clerk"}
		if result, err := env.executor.Execute(context.Background(), memberClaim(t, command)); err != nil || result.State != contract.OperationSucceeded {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		var roles, fullName string
		var enabled bool
		if err := env.database.QueryRow(`SELECT roles, full_name, enabled FROM _kora_user WHERE name = 'usr_member_1'`).Scan(&roles, &fullName, &enabled); err != nil {
			t.Fatal(err)
		}
		if roles != "Stock Clerk" || fullName != "Amina Otieno" || !enabled {
			t.Fatalf("roles=%q full_name=%q enabled=%v", roles, fullName, enabled)
		}
		if got := env.count(t, `SELECT COUNT(*) FROM _kora_user WHERE site = ?`, grantTestSite); got != 1 {
			t.Fatalf("%d rows", got)
		}
	})
	t.Run("enabled member is never changed", func(t *testing.T) {
		env := newMemberTestEnv(t)
		env.insertUser(t, "usr_member_1", "cashier@example.test", site.PasswordlessPasswordHash, true, "Administrator")
		revision := env.membershipRevision(t)
		if result, err := env.executor.Execute(context.Background(), memberClaim(t, memberGrant())); err != nil || result.State != contract.OperationSucceeded {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		var roles string
		if err := env.database.QueryRow(`SELECT roles FROM _kora_user WHERE name = 'usr_member_1'`).Scan(&roles); err != nil || roles != "Administrator" {
			t.Fatalf("roles=%q err=%v: a grant must not change an enabled member", roles, err)
		}
		if got := env.membershipRevision(t); got != revision {
			t.Fatalf("no-op grant bumped membership revision %d -> %d", revision, got)
		}
	})
}

func TestManagementMemberGrantInfrastructureFailuresNeedRecovery(t *testing.T) {
	env := newMemberTestEnv(t)
	env.executor.acquire = func(context.Context, contract.TenantID) (managementMemberRuntime, func(), error) {
		return managementMemberRuntime{}, nil, errors.New("tenant runtime is unavailable")
	}
	result, err := env.executor.Execute(context.Background(), memberClaim(t, memberGrant()))
	if err != nil || result.State != contract.OperationRecoveryRequired || result.Error == nil || result.Error.Type != contract.CodeDependencyUnavailable {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestManagementMemberGrantRejectsForeignOrInvalidOperations(t *testing.T) {
	env := newMemberTestEnv(t)
	other := memberClaim(t, memberGrant())
	other.Operation.Kind = management.CommandSetTenantState
	invalid := memberClaim(t, memberGrant())
	invalid.Operation.Command = json.RawMessage(`{"subject":"../x"}`)
	mismatch := memberGrant()
	mismatch.Meta.IdempotencyKey = "other-key-0001"
	for name, claim := range map[string]management.Claim{"other kind": other, "invalid command": invalid, "key differs from invitation": memberClaim(t, mismatch)} {
		result, err := env.executor.Execute(context.Background(), claim)
		if err != nil || result.State != contract.OperationFailed || result.Error == nil || result.Error.Type != contract.CodeValidationFailed {
			t.Fatalf("%s: result=%+v err=%v", name, result, err)
		}
	}
	unfenced := memberClaim(t, memberGrant())
	unfenced.Fence = 0
	if _, err := env.executor.Execute(context.Background(), unfenced); err == nil {
		t.Fatal("unfenced claim executed")
	}
	if got := env.count(t, `SELECT COUNT(*) FROM _kora_user`); got != 0 {
		t.Fatalf("%d rows created by refused operations", got)
	}
}

func TestManagementMemberPlanReportsTheSameProblems(t *testing.T) {
	tests := []struct {
		name string
		seed func(*testing.T, *memberTestEnv)
		req  contract.PlanTenantMember
		want []contract.TenantMemberProblemCode
	}{
		{name: "allowed", req: memberPlanRequest("cashier@example.test", "Cashier")},
		{name: "administrator", req: memberPlanRequest("cashier@example.test", "Administrator"), want: []contract.TenantMemberProblemCode{contract.TenantMemberRoleNotAssignable}},
		{name: "unknown role", req: memberPlanRequest("cashier@example.test", "Auditor"), want: []contract.TenantMemberProblemCode{contract.TenantMemberRoleUnknown}},
		{
			name: "existing cloud member", req: memberPlanRequest("Cashier@example.test", "Cashier"), want: []contract.TenantMemberProblemCode{contract.TenantMemberAlreadyExists},
			seed: func(t *testing.T, e *memberTestEnv) {
				e.insertUser(t, "usr_member_1", "cashier@example.test", site.PasswordlessPasswordHash, true, "Cashier")
			},
		},
		{
			name: "local password user", req: memberPlanRequest("cashier@example.test", "Cashier"), want: []contract.TenantMemberProblemCode{contract.TenantMemberEmailInUse},
			seed: func(t *testing.T, e *memberTestEnv) {
				e.insertUser(t, "local-user", "cashier@example.test", "$2a$10$hash", false, "")
			},
		},
		{
			name: "revoked cloud member may be invited again", req: memberPlanRequest("cashier@example.test", "Cashier"),
			seed: func(t *testing.T, e *memberTestEnv) {
				e.insertUser(t, "usr_member_1", "cashier@example.test", site.PasswordlessPasswordHash, false, "")
			},
		},
		{
			name: "role and email problems together", req: memberPlanRequest("cashier@example.test", "Auditor"),
			want: []contract.TenantMemberProblemCode{contract.TenantMemberRoleUnknown, contract.TenantMemberEmailInUse},
			seed: func(t *testing.T, e *memberTestEnv) {
				e.insertUser(t, "local-user", "cashier@example.test", "$2a$10$hash", true, "")
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			env := newMemberTestEnv(t)
			if test.seed != nil {
				test.seed(t, env)
			}
			rows := env.count(t, `SELECT COUNT(*) FROM _kora_user`)
			plan, err := env.executor.PlanMember(context.Background(), "tenant-a", test.req)
			if err != nil {
				t.Fatal(err)
			}
			if err := contract.ValidateTenantMemberPlan(plan); err != nil {
				t.Fatalf("invalid plan %+v: %v", plan, err)
			}
			var got []contract.TenantMemberProblemCode
			for _, problem := range plan.Problems {
				got = append(got, problem.Code)
			}
			if plan.Allowed != (len(test.want) == 0) || strings.Join(codeStrings(got), ",") != strings.Join(codeStrings(test.want), ",") {
				t.Fatalf("plan=%+v want %v", plan, test.want)
			}
			if env.count(t, `SELECT COUNT(*) FROM _kora_user`) != rows || env.count(t, `SELECT COUNT(*) FROM _kora_operation_audit`) != 0 || env.released != 1 {
				t.Fatal("plan changed tenant state or leaked the runtime handle")
			}
		})
	}
	env := newMemberTestEnv(t)
	if _, err := env.executor.PlanMember(context.Background(), "tenant-missing", memberPlanRequest("cashier@example.test", "Cashier")); err == nil {
		t.Fatal("plan for an inactive tenant succeeded")
	}
}

func memberPlanRequest(email, role string) contract.PlanTenantMember {
	return contract.PlanTenantMember{Meta: contract.CommandMeta{IdempotencyKey: "member-plan-01"}, Email: email, Roles: []string{role}}
}

func codeStrings(codes []contract.TenantMemberProblemCode) []string {
	out := make([]string, len(codes))
	for i, code := range codes {
		out[i] = string(code)
	}
	return out
}

type memberAdmissionStub struct{}

func (memberAdmissionStub) Acquire(context.Context) (func(), error) { return func() {}, nil }

type memberOnlyExecutorStub struct{ calls int }

func (s *memberOnlyExecutorStub) Execute(context.Context, management.Claim) (management.FinishResult, error) {
	s.calls++
	return management.FinishResult{State: contract.OperationSucceeded}, nil
}

func TestManagementDispatcherRoutesMemberGrantOnlyWhenWired(t *testing.T) {
	claim := memberClaim(t, memberGrant())
	if result, err := (managementOperationDispatcher{}).Execute(context.Background(), claim); err != nil || result.State != contract.OperationFailed {
		t.Fatalf("unwired dispatcher result=%+v err=%v", result, err)
	}
	stub := &memberOnlyExecutorStub{}
	if result, err := (managementOperationDispatcher{memberGrant: stub}).Execute(context.Background(), claim); err != nil || result.State != contract.OperationSucceeded || stub.calls != 1 {
		t.Fatalf("wired dispatcher result=%+v err=%v calls=%d", result, err, stub.calls)
	}
}

func TestManagementRuntimeAdvertisesTenantMemberCapabilities(t *testing.T) {
	platform, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "platform.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = platform.Close() })
	runtime, err := assembleManagementOperationRuntime(context.Background(), managementRuntimeOptions{token: "token", caller: "cloud-control-plane", database: platform, dialect: db.Resolve("libsql"), admission: memberAdmissionStub{}, directory: &managementStateDirectoryStub{}, coordinator: &managementStateCoordinatorStub{}, tenants: knet.NewSiteRouter(nil), platformDBType: "libsql", common: &site.CommonConfig{DBType: "libsql"}, deploymentID: "deployment-a"})
	if err != nil {
		t.Fatal(err)
	}
	if !runtime.routes.Capabilities[api.ManagementMemberPlanCapability] || !runtime.routes.Capabilities[api.ManagementMemberGrantCapability] || runtime.routes.MemberPlanner == nil {
		t.Fatalf("member capabilities were not composed: %+v", runtime.routes.Capabilities)
	}
}

func TestAssembleManagementOperationRuntimeOptionDefaults(t *testing.T) {
	database := new(sql.DB)
	complete := managementRuntimeOptions{token: "token", caller: "cloud-control-plane", database: database, dialect: db.Resolve("libsql"), admission: memberAdmissionStub{}}
	tests := []struct {
		name        string
		mutate      func(*managementRuntimeOptions)
		wantRuntime bool
		wantErr     bool
	}{
		{name: "no caller leaves the runtime in inspection-only mode", mutate: func(o *managementRuntimeOptions) { *o = managementRuntimeOptions{} }},
		{name: "no token", mutate: func(o *managementRuntimeOptions) { o.token = "" }, wantErr: true},
		{name: "no database", mutate: func(o *managementRuntimeOptions) { o.database = nil }, wantErr: true},
		{name: "no dialect", mutate: func(o *managementRuntimeOptions) { o.dialect = nil }, wantErr: true},
		{name: "no admission", mutate: func(o *managementRuntimeOptions) { o.admission = nil }, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := complete
			tt.mutate(&opts)
			runtime, err := assembleManagementOperationRuntime(context.Background(), opts)
			if (err != nil) != tt.wantErr || (runtime != nil) != tt.wantRuntime {
				t.Fatalf("assembleManagementOperationRuntime = (%v, %v), want runtime=%t error=%t", runtime, err, tt.wantRuntime, tt.wantErr)
			}
		})
	}
}
