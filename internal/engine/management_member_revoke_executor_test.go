package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/auth"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/management"
	"github.com/asenawritescode/kora/internal/site"
	_ "github.com/mattn/go-sqlite3"
)

type memberTestAdmission struct{}

func (memberTestAdmission) Acquire(context.Context) (func(), error) { return func() {}, nil }

type memberFixture struct {
	database *sql.DB
	sessions *auth.SessionManager
	executor *managementMemberRevokeExecutor
}

const memberTestSite = "shop.example.test"

func newMemberFixture(t *testing.T) *memberFixture {
	t.Helper()
	database, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "member.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	dialect := db.Resolve("libsql")
	if err = site.BootstrapSystemTables(context.Background(), database, dialect); err != nil {
		t.Fatal(err)
	}
	sessions := auth.NewOwnedSessionManager(database).SetDialect(dialect)
	t.Cleanup(func() { _ = sessions.Close() })
	runtime := managementMemberRuntime{Site: memberTestSite, DB: database, Dialect: dialect, Sessions: sessions, Admission: memberTestAdmission{}}
	executor, err := newManagementMemberRevokeExecutor(func(_ context.Context, tenant contract.TenantID) (managementMemberRuntime, func(), error) {
		if tenant != "tenant-123456789" {
			return managementMemberRuntime{}, nil, errors.New("tenant unavailable")
		}
		return runtime, func() {}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return &memberFixture{database: database, sessions: sessions, executor: executor}
}

func (f *memberFixture) addUser(t *testing.T, siteName, name, roles string, enabled bool) {
	t.Helper()
	if _, err := f.database.Exec(`INSERT INTO _kora_user (name, site, email, password_hash, full_name, enabled, roles) VALUES (?, ?, ?, '', ?, ?, ?)`, name, siteName, name+"@example.test", name, enabled, roles); err != nil {
		t.Fatal(err)
	}
}

func (f *memberFixture) addSession(t *testing.T, siteName, name string) {
	t.Helper()
	if _, err := f.database.Exec(`INSERT INTO _kora_session (sid, site, "user", data, expires_at) VALUES (?, ?, ?, '{}', '2999-01-01 00:00:00')`, siteName+"/"+name, siteName, name); err != nil {
		t.Fatal(err)
	}
}

func (f *memberFixture) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := f.database.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func revokeClaim(t *testing.T, subject string) management.Claim {
	t.Helper()
	command, err := json.Marshal(contract.RevokeTenantMember{Meta: contract.CommandMeta{IdempotencyKey: "revoke-" + contract.IdempotencyKey(subject)}, Subject: contract.PrincipalID(subject), Reason: "removed from team"})
	if err != nil {
		t.Fatal(err)
	}
	return management.Claim{Owner: "worker-a", Fence: 1, Operation: management.Operation{ID: "operation-revoke-1", Tenant: "tenant-123456789", Kind: management.CommandTenantMemberRevoke, Command: command}}
}

func TestMemberRevokeDisablesClearsRolesAndDropsSessionsKeepingRow(t *testing.T) {
	for _, test := range []struct {
		name  string
		roles string
	}{
		{"cashier", "Cashier"},
		{"already revoked shape", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newMemberFixture(t)
			f.addUser(t, memberTestSite, "cloud-user-1", test.roles, test.roles != "")
			f.addUser(t, memberTestSite, "cloud-user-2", "Cashier", true)
			f.addUser(t, "other.example.test", "cloud-user-1x", "Cashier", true)
			f.addSession(t, memberTestSite, "cloud-user-1")
			f.addSession(t, memberTestSite, "cloud-user-2")
			f.addSession(t, "other.example.test", "cloud-user-1x")
			before := f.count(t, `SELECT membership_revision FROM _kora_auth_revision WHERE id = 1`)

			// Twice: the second run is the idempotent replay of a recovered claim.
			for i := 0; i < 2; i++ {
				result, err := f.executor.Execute(context.Background(), revokeClaim(t, "cloud-user-1"))
				if err != nil || result.State != contract.OperationSucceeded {
					t.Fatalf("run %d: result=%+v err=%v", i, result, err)
				}
			}
			var enabled bool
			var roles string
			if err := f.database.QueryRow(`SELECT enabled, COALESCE(roles, '') FROM _kora_user WHERE site = ? AND name = ?`, memberTestSite, "cloud-user-1").Scan(&enabled, &roles); err != nil {
				t.Fatalf("the user row must be kept: %v", err)
			}
			if enabled || roles != "" {
				t.Fatalf("enabled=%v roles=%q, want disabled with no roles", enabled, roles)
			}
			if n := f.count(t, `SELECT COUNT(*) FROM _kora_session WHERE site = ? AND "user" = ?`, memberTestSite, "cloud-user-1"); n != 0 {
				t.Fatalf("revoked member kept %d sessions", n)
			}
			if n := f.count(t, `SELECT COUNT(*) FROM _kora_session`); n != 2 {
				t.Fatalf("other users' sessions = %d, want 2", n)
			}
			if n := f.count(t, `SELECT COUNT(*) FROM _kora_user WHERE enabled = 1`); n != 2 {
				t.Fatalf("other users were touched: enabled=%d", n)
			}
			if after := f.count(t, `SELECT membership_revision FROM _kora_auth_revision WHERE id = 1`); after <= before {
				t.Fatalf("membership revision %d did not advance from %d", after, before)
			}
			if _, err := f.sessions.FindUserByCloudSubject(memberTestSite, "cloud-user-1"); !errors.Is(err, auth.ErrDisabledAccount) {
				t.Fatalf("handoff lookup error = %v, want disabled account", err)
			}
		})
	}
}

func TestMemberRevokeGuardsAndFailures(t *testing.T) {
	for _, test := range []struct {
		name         string
		seed         func(*testing.T, *memberFixture)
		subject      string
		wantState    contract.OperationState
		wantCode     contract.Code
		wantEnabled  bool // of the subject afterwards, when the row exists
		wantSessions int
	}{
		{
			name: "unknown subject fails cleanly",
			seed: func(t *testing.T, f *memberFixture) {
				f.addUser(t, memberTestSite, "cloud-admin-1", doctype.AdminRole, true)
			},
			subject: "cloud-ghost-9", wantState: contract.OperationFailed, wantCode: contract.CodeNotFound,
		},
		{
			name: "subject of another tenant is unknown here",
			seed: func(t *testing.T, f *memberFixture) {
				f.addUser(t, memberTestSite, "cloud-admin-1", doctype.AdminRole, true)
				f.addUser(t, "other.example.test", "cloud-user-7", "Cashier", true)
			},
			subject: "cloud-user-7", wantState: contract.OperationFailed, wantCode: contract.CodeNotFound,
		},
		{
			name: "last enabled administrator is refused and untouched",
			seed: func(t *testing.T, f *memberFixture) {
				f.addUser(t, memberTestSite, "cloud-admin-1", doctype.AdminRole, true)
				f.addUser(t, memberTestSite, "cloud-admin-2", doctype.AdminRole, false)
				f.addUser(t, memberTestSite, "cloud-user-2", "Cashier", true)
				f.addSession(t, memberTestSite, "cloud-admin-1")
			},
			subject: "cloud-admin-1", wantState: contract.OperationFailed, wantCode: contract.CodeConflict, wantEnabled: true, wantSessions: 1,
		},
		{
			name: "administrator may be revoked while another enabled administrator remains",
			seed: func(t *testing.T, f *memberFixture) {
				f.addUser(t, memberTestSite, "cloud-admin-1", doctype.AdminRole, true)
				f.addUser(t, memberTestSite, "cloud-admin-2", "Cashier,"+doctype.AdminRole, true)
				f.addSession(t, memberTestSite, "cloud-admin-1")
			},
			subject: "cloud-admin-1", wantState: contract.OperationSucceeded,
		},
		{
			name: "disabled administrator is not the last enabled one",
			seed: func(t *testing.T, f *memberFixture) {
				f.addUser(t, memberTestSite, "cloud-admin-1", doctype.AdminRole, false)
			},
			subject: "cloud-admin-1", wantState: contract.OperationSucceeded,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newMemberFixture(t)
			test.seed(t, f)
			result, err := f.executor.Execute(context.Background(), revokeClaim(t, test.subject))
			if err != nil || result.State != test.wantState {
				t.Fatalf("result=%+v err=%v, want state %s", result, err, test.wantState)
			}
			if test.wantCode != "" && (result.Error == nil || result.Error.Type != test.wantCode) {
				t.Fatalf("error = %+v, want code %s", result.Error, test.wantCode)
			}
			if test.wantState == contract.OperationFailed && test.wantCode == contract.CodeConflict {
				var enabled bool
				if err := f.database.QueryRow(`SELECT enabled FROM _kora_user WHERE name = ?`, test.subject).Scan(&enabled); err != nil || enabled != test.wantEnabled {
					t.Fatalf("subject enabled=%v err=%v after refusal", enabled, err)
				}
				if n := f.count(t, `SELECT COUNT(*) FROM _kora_session WHERE "user" = ?`, test.subject); n != test.wantSessions {
					t.Fatalf("sessions after refusal = %d, want %d", n, test.wantSessions)
				}
			}
		})
	}
}

func TestMemberRevokeExecutorRejectsUnsafeClaims(t *testing.T) {
	f := newMemberFixture(t)
	f.addUser(t, memberTestSite, "cloud-user-1", "Cashier", true)
	wrongKind := revokeClaim(t, "cloud-user-1")
	wrongKind.Operation.Kind = management.CommandTenantInvitation
	badCommand := revokeClaim(t, "cloud-user-1")
	badCommand.Operation.Command = json.RawMessage(`{"meta":{"idempotency_key":"revoke-1"},"subject":"..","reason":""}`)
	unavailable := revokeClaim(t, "cloud-user-1")
	unavailable.Operation.Tenant = "tenant-other-0001"
	for _, test := range []struct {
		name     string
		claim    management.Claim
		wantCode contract.Code
	}{
		{"wrong kind", wrongKind, contract.CodeValidationFailed},
		{"invalid stored command", badCommand, contract.CodeValidationFailed},
		{"tenant runtime unavailable", unavailable, contract.CodeDependencyUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, err := f.executor.Execute(context.Background(), test.claim)
			if err != nil || result.State != contract.OperationFailed || result.Error == nil || result.Error.Type != test.wantCode {
				t.Fatalf("result=%+v err=%v, want failed %s", result, err, test.wantCode)
			}
			if strings.Contains(result.Error.Message, "cloud-user-1") {
				t.Fatalf("failure message leaks the subject: %q", result.Error.Message)
			}
		})
	}
	unfenced := revokeClaim(t, "cloud-user-1")
	unfenced.Fence = 0
	if _, err := f.executor.Execute(context.Background(), unfenced); err == nil {
		t.Fatal("an unfenced claim must be refused")
	}
	if n := f.count(t, `SELECT COUNT(*) FROM _kora_user WHERE enabled = 1`); n != 1 {
		t.Fatalf("a rejected claim changed the user: enabled=%d", n)
	}
}
