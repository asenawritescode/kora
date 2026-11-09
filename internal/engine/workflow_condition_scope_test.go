package engine

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	knet "github.com/asenawritescode/kora/internal/net"
	"github.com/asenawritescode/kora/internal/policy"
)

func TestTenantWorkflowConditionScopeRequiresGrantAndProjectsCurrentVisibleFields(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("create SQL mock:", err)
	}
	defer database.Close()
	dt := &doctype.DocType{Name: "Invoice", Fields: []doctype.Field{
		{Fieldname: "title", Fieldtype: "Data"},
		{Fieldname: "secret", Fieldtype: "Data", Hidden: true},
		{Fieldname: "amount", Fieldtype: "Currency"},
		{Fieldname: "password", Fieldtype: "Password"},
	}}
	registry := doctype.NewRegistry()
	registry.LoadFull([]*doctype.DocType{dt}, nil, nil)
	runtime := &knet.LoadedSite{SiteID: "tenant-a", Name: "tenant-a.example.test", DB: database, Registry: registry}
	provider, err := newTenantWorkflowConditionScope(runtime, db.Resolve("mysql"))
	if err != nil {
		t.Fatal("construct condition scope:", err)
	}
	instance := contract.WorkflowInstance{Subject: contract.SubjectRef{Kind: "Invoice", ID: "INV-1", Revision: "7"}}
	service := func(grants ...policy.Grant) policy.Execution {
		exec, err := policy.NewVerifiedExecutionWithGrants("tenant-a", contract.ActorContext{PrincipalID: "workflow-service", PrincipalType: contract.PrincipalService,
			Site: runtime.Name, AuthenticatedAt: time.Now().UTC()}, grants)
		if err != nil {
			t.Fatal("construct service execution:", err)
		}
		return exec
	}
	if _, err := provider.Scope(context.Background(), service(), instance); err == nil {
		t.Fatal("scope accepted missing subject read grant")
	}
	if _, err := provider.Scope(context.Background(), service(policy.Grant{Resource: "Invoice", Operation: "read", OwnerOnly: true}), instance); err == nil {
		t.Fatal("scope accepted owner-only grant without a verified human owner")
	}
	query := "SELECT title, secret, amount, password, name, owner, creation, modified, modified_by, doc_status, revision FROM `tabInvoice` WHERE name = ?"
	mock.ExpectQuery(regexp.QuoteMeta(query)).WithArgs("INV-1").WillReturnRows(sqlmock.NewRows([]string{
		"title", "secret", "amount", "password", "name", "owner", "creation", "modified", "modified_by", "doc_status", "revision",
	}).AddRow("Office chairs", "classified", "42.5", "secret-hash", "INV-1", "user-1", time.Now(), time.Now(), "user-1", 0, 7))
	scope, err := provider.Scope(context.Background(), service(policy.Grant{Resource: "Invoice", Operation: "read"}), instance)
	if err != nil {
		t.Fatal("load authorized scope:", err)
	}
	if got, ok := scope.Lookup("doc.amount"); !ok || got.Kind != "number" || got.Number != 42.5 {
		t.Fatalf("amount scope = (%+v,%t), want numeric amount", got, ok)
	}
	if got, ok := scope.Lookup("doc.title"); !ok || got.String != "Office chairs" {
		t.Fatalf("title scope = (%+v,%t), want visible title", got, ok)
	}
	for _, path := range []string{"doc.secret", "doc.password", "amount", "doc.owner"} {
		if _, ok := scope.Lookup(path); ok {
			t.Errorf("scope exposed unauthorized/unqualified path %q", path)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("SQL expectations:", err)
	}
}

func TestTenantWorkflowConditionScopeRejectsStaleSubjectRevision(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("create SQL mock:", err)
	}
	defer database.Close()
	registry := doctype.NewRegistry()
	registry.LoadFull([]*doctype.DocType{{Name: "Invoice", Fields: []doctype.Field{{Fieldname: "amount", Fieldtype: "Currency"}}}}, nil, nil)
	runtime := &knet.LoadedSite{SiteID: "tenant-a", Name: "tenant-a.example.test", DB: database, Registry: registry}
	provider, err := newTenantWorkflowConditionScope(runtime, db.Resolve("mysql"))
	if err != nil {
		t.Fatal("construct condition scope:", err)
	}
	mock.ExpectQuery(regexp.QuoteMeta("SELECT amount, name, owner, creation, modified, modified_by, doc_status, revision FROM `tabInvoice` WHERE name = ?")).WithArgs("INV-1").WillReturnRows(sqlmock.NewRows([]string{"amount", "name", "owner", "creation", "modified", "modified_by", "doc_status", "revision"}).AddRow("2", "INV-1", "user-1", time.Now(), time.Now(), "user-1", 0, 8))
	exec, err := policy.NewVerifiedExecutionWithGrants("tenant-a", contract.ActorContext{PrincipalID: "workflow-service", PrincipalType: contract.PrincipalService,
		Site: runtime.Name, AuthenticatedAt: time.Now().UTC()}, []policy.Grant{{Resource: "Invoice", Operation: "read"}})
	if err != nil {
		t.Fatal("construct service execution:", err)
	}
	_, err = provider.Scope(context.Background(), exec, contract.WorkflowInstance{Subject: contract.SubjectRef{Kind: "Invoice", ID: "INV-1", Revision: "7"}})
	var typed *contract.Error
	if !errors.As(err, &typed) || typed.Type != contract.CodeStaleSubject {
		t.Fatalf("stale subject error = %v, want STALE_SUBJECT", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("SQL expectations:", err)
	}
}
