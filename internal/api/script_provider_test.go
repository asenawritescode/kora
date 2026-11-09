package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/orm"
	"github.com/asenawritescode/kora/internal/policy"
	"github.com/asenawritescode/kora/internal/script"
)

type externalJournalTest struct {
	calls int
	err   error
}

func (j *externalJournalTest) BeforeExternalEffect(context.Context) error {
	j.calls++
	return j.err
}

type unexpectedHTTPTransport struct{ called bool }

func (t *unexpectedHTTPTransport) RoundTrip(*http.Request) (*http.Response, error) {
	t.called = true
	return nil, errors.New("network must not be reached")
}

func TestScriptProviderFactoryRebindsOperationTransaction(t *testing.T) {
	registry := doctype.NewRegistry()
	factory := ScriptProviderFactory(registry, "tenant-a", nil, nil)
	first := &orm.TxManager{SiteName: "tenant-a"}
	second := &orm.TxManager{SiteName: "tenant-a"}
	firstProvider := factory(first).(*scriptProvider)
	secondProvider := factory(second).(*scriptProvider)
	if firstProvider.tx != first || secondProvider.tx != second || firstProvider == secondProvider {
		t.Fatal("provider factory did not produce distinct operation-scoped providers")
	}
	if firstProvider.httpClient != secondProvider.httpClient {
		t.Fatal("scoped providers should share the tenant-owned HTTP client")
	}
	firstProvider.Close()
}

func TestScriptProviderMutationRejectsCallerRolesWithoutVerifiedExecution(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("create SQL mock:", err)
	}
	defer database.Close()
	registry := doctype.NewRegistry()
	registry.Register(&doctype.DocType{Name: "Task", Fields: []doctype.Field{{Fieldname: "title", Fieldtype: "Data"}}})
	tx := &orm.TxManager{
		DB: database, Registry: registry, Dialect: db.Resolve("mysql"), SiteName: "tenant-a",
		CurrentUser: "caller", CurrentUserRole: doctype.AdminRole, CurrentUserRoles: []string{doctype.AdminRole},
	}
	provider := NewScriptProvider(tx, registry, "tenant-a", nil, nil)
	if _, err := provider.CreateDoc("Task", map[string]any{"title": "should be denied"}, "caller", "caller"); err == nil {
		t.Fatal("script mutation accepted caller-supplied Administrator role without verified execution")
	} else if commandErr, ok := err.(*contract.Error); !ok || commandErr.Type != contract.CodeUnauthenticated {
		t.Fatalf("script mutation error = %#v, want UNAUTHENTICATED", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unverified script mutation performed database work: %v", err)
	}
}

func TestScriptProviderRequiresDurableMarkerBeforeExternalHTTP(t *testing.T) {
	providerValue := NewScriptProvider(&orm.TxManager{}, doctype.NewRegistry(), "tenant-a", nil, []string{"*"})
	provider, ok := providerValue.(*scriptProvider)
	if !ok {
		t.Fatal("script provider has unexpected implementation")
	}
	transport := &unexpectedHTTPTransport{}
	provider.httpClient = &http.Client{Transport: transport}
	journal := &externalJournalTest{err: errors.New("claim fence lost")}
	var scoped script.ExternalEffectScopedProvider = provider
	journalProvider := scoped.WithExternalEffectJournal(journal).(script.HTTPProvider)
	_, err := journalProvider.DoHTTP(&script.HTTPRequest{Method: http.MethodPost, URL: "https://example.com/effect"})
	if err == nil || journal.calls != 1 || transport.called {
		t.Fatalf("HTTP effect result err=%v journal calls=%d network called=%t; want marked failure and no dispatch", err, journal.calls, transport.called)
	}
}

func TestLifecycleScopedProviderSkipsItsOwnScriptOnNestedMutation(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("create SQL mock:", err)
	}
	defer database.Close()
	registry := doctype.NewRegistry()
	registry.Register(&doctype.DocType{Name: "Task", Fields: []doctype.Field{{Fieldname: "title", Fieldtype: "Data"}}})
	actor := contract.ActorContext{PrincipalID: "user-1", SubjectUserID: "alice@example.test", PrincipalType: contract.PrincipalHuman, Site: "tenant-a", AuthenticatedAt: time.Now().UTC()}
	execution, err := policy.NewVerifiedExecution(contract.TenantID("tenant-a"), actor)
	if err != nil {
		t.Fatal("create verified execution:", err)
	}
	tx := &orm.TxManager{DB: database, Registry: registry, Dialect: db.Resolve("mysql"), SiteName: "tenant-a", Execution: execution, CurrentUser: actor.SubjectUserID}
	var mutation script.MutationRequest
	provider := NewScriptProvider(tx, registry, "tenant-a", nil, nil)
	provider = provider.(script.LifecycleScopedProvider).WithLifecycleHook("Task", script.EventAfterSave, "task-after-save")
	provider = provider.(script.MutationScopedProvider).WithMutationExecutor(script.MutationExecutorFunc(func(_ context.Context, request script.MutationRequest) (json.RawMessage, error) {
		mutation = request
		return json.RawMessage(`{"doctype":"Task","name":"TASK-1","created":true,"document":{"name":"TASK-1"}}`), nil
	}))
	if _, err := provider.CreateDoc("Task", map[string]any{"title": "Nested"}, "caller", actor.SubjectUserID); err != nil {
		t.Fatal("execute nested mutation through provider:", err)
	}
	if len(mutation.SkipHookScripts) != 1 || mutation.SkipHookScripts[0] != "task-after-save" {
		t.Fatalf("nested mutation skip-hook list = %#v, want the currently executing script", mutation.SkipHookScripts)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("unexpected provider database access:", err)
	}
}

func TestScriptProviderReadRequiresVerifiedAndAuthorizedIdentity(t *testing.T) {
	registry := doctype.NewRegistry()
	registry.Register(&doctype.DocType{Name: "Invoice", Fields: []doctype.Field{{Fieldname: "assigned_to", Fieldtype: "User"}}})
	registry.Permissions.AddRole(&doctype.Role{Name: "OwnerReader"})
	registry.Permissions.AddRole(&doctype.Role{Name: "AssigneeReader"})
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: "Invoice", Role: "OwnerReader", Read: true, IfOwner: true})
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: "Invoice", Role: "AssigneeReader", Read: true, IfAssigned: []string{"assigned_to"}})
	for _, tc := range []struct {
		name      string
		actor     contract.ActorContext
		grants    []policy.Grant
		want      string
		wantField string
		deny      bool
	}{
		{name: "unverified", deny: true},
		{name: "verified role is not authorized", actor: contract.ActorContext{PrincipalID: "alice", PrincipalType: contract.PrincipalHuman, Site: "tenant-a", Roles: []string{"Writer"}}, deny: true},
		{name: "owner-scoped role", actor: contract.ActorContext{PrincipalID: "alice", SubjectUserID: "alice", PrincipalType: contract.PrincipalHuman, Site: "tenant-a", Roles: []string{"OwnerReader"}}, want: "alice"},
		{name: "assignment-scoped role", actor: contract.ActorContext{PrincipalID: "alice", SubjectUserID: "alice", PrincipalType: contract.PrincipalHuman, Site: "tenant-a", Roles: []string{"AssigneeReader"}}, want: "alice", wantField: "assigned_to"},
		{name: "owner-scoped delegated grant", actor: contract.ActorContext{PrincipalID: "extension:reporter", PrincipalType: contract.PrincipalService, Site: "tenant-a"}, grants: []policy.Grant{{Resource: "Invoice", Operation: "read", OwnerOnly: true}}, want: "extension:reporter"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			execution := policy.Execution{}
			if tc.actor.Authenticated() {
				var err error
				execution, err = policy.NewVerifiedExecutionWithGrants("tenant-a", tc.actor, tc.grants)
				if err != nil {
					t.Fatal("create execution:", err)
				}
			}
			provider := &scriptProvider{site: "tenant-a", registry: registry, tx: &orm.TxManager{Execution: execution}}
			scope, err := provider.authorizeRead(registry.Get("Invoice"))
			if tc.deny {
				if err == nil {
					t.Fatal("unauthorized script read was allowed")
				}
				return
			}
			if err != nil || scope.UserID != tc.want || (tc.want != "" && tc.wantField == "" && !scope.Owner) || (tc.wantField != "" && (len(scope.AssignedFields) != 1 || scope.AssignedFields[0] != tc.wantField)) {
				t.Fatalf("authorizeRead = (%#v, %v), want owner %q", scope, err, tc.want)
			}
		})
	}
}

func TestScriptReadProjectionExcludesHiddenAndPasswordFieldsRecursively(t *testing.T) {
	registry := doctype.NewRegistry()
	registry.Register(&doctype.DocType{Name: "Line", IsChildTable: true, Fields: []doctype.Field{
		{Fieldname: "label", Fieldtype: "Data"},
		{Fieldname: "internal_cost", Fieldtype: "Currency", Hidden: true},
		{Fieldname: "credential", Fieldtype: "Password"},
	}})
	registry.Register(&doctype.DocType{Name: "Invoice", Fields: []doctype.Field{
		{Fieldname: "title", Fieldtype: "Data"},
		{Fieldname: "private_note", Fieldtype: "Data", Hidden: true},
		{Fieldname: "credential", Fieldtype: "Password"},
		{Fieldname: "lines", Fieldtype: "Table", Options: "Line"},
	}})
	got := projectScriptReadDocument(registry.Get("Invoice"), registry, map[string]any{
		"name": "INV-1", "owner": "reader", "title": "Visible", "private_note": "hidden", "credential": "password",
		"lines": []map[string]any{{"name": "LINE-1", "label": "Visible line", "internal_cost": 17, "credential": "nested password"}},
	})
	if got["name"] != "INV-1" || got["owner"] != "reader" || got["title"] != "Visible" {
		t.Fatalf("script visible document metadata/data missing: %#v", got)
	}
	for _, forbidden := range []string{"private_note", "credential"} {
		if _, exists := got[forbidden]; exists {
			t.Fatalf("script document projection exposed %q: %#v", forbidden, got)
		}
	}
	lines, ok := got["lines"].([]any)
	if !ok || len(lines) != 1 {
		t.Fatalf("script child rows projection = %#v", got["lines"])
	}
	line, ok := lines[0].(map[string]any)
	if !ok || line["label"] != "Visible line" {
		t.Fatalf("script visible child row = %#v", lines[0])
	}
	if _, exists := line["internal_cost"]; exists {
		t.Fatalf("script child projection exposed hidden field: %#v", line)
	}
	if _, exists := line["credential"]; exists {
		t.Fatalf("script child projection exposed password field: %#v", line)
	}
}

func TestMapToFilterStringEscapesValuesAsJSON(t *testing.T) {
	got, err := mapToFilterString(map[string]any{"title": `x","name":"other`})
	if err != nil {
		t.Fatal("encode filter:", err)
	}
	var filters [][]any
	if err := json.Unmarshal([]byte(got), &filters); err != nil {
		t.Fatalf("invalid filter JSON %q: %v", got, err)
	}
	if len(filters) != 1 || filters[0][0] != "title" || filters[0][2] != `x","name":"other` {
		t.Fatalf("filter value changed shape: %#v", filters)
	}
}
