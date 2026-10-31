package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/policy"
	"github.com/gin-gonic/gin"
)

func TestHandleRecordHistoryRequiresReadableRecordAndReturnsRedactedActivity(t *testing.T) {
	handler, _, mock, database := setupTestHandler(t)
	defer database.Close()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/v1/resource/TestDoc/REC-1/history", nil)
	ctx.Params = gin.Params{{Key: "doctype", Value: "TestDoc"}, {Key: "name", Value: "REC-1"}}
	ctx.Set("site_name", "test.local")
	ctx.Set("site_db", database)
	ctx.Set("policy_execution", routerTestExecution([]string{"Administrator"}, nil))
	ctx.Set("user", "admin")
	ctx.Set("user_roles", []string{"Administrator"})
	handler.Registry.Get("TestDoc").Fields = append(handler.Registry.Get("TestDoc").Fields,
		doctype.Field{Fieldname: "private_note", Fieldtype: "Data", Label: "Private note", Hidden: true},
		doctype.Field{Fieldname: "credential", Fieldtype: "Password", Label: "Credential"},
	)
	handler.Registry.Register(handler.Registry.Get("TestDoc"))

	stamp := time.Date(2026, time.October, 28, 10, 30, 0, 0, time.UTC)
	mock.ExpectQuery("SELECT title, private_note, credential, name, owner, creation, modified, modified_by, doc_status, revision FROM `tabTestDoc` WHERE name = \\?").
		WithArgs("REC-1").
		WillReturnRows(mock.NewRows([]string{"title", "private_note", "credential", "name", "owner", "creation", "modified", "modified_by", "doc_status", "revision"}).
			AddRow("Example", "private-current", "password-current", "REC-1", "admin", stamp, stamp, "admin", 0, 1))
	mock.ExpectQuery("SELECT a.actor_user, a.command_name, a.created_at, h.changes_json FROM _kora_operation_audit a").
		WithArgs("test.local", "TestDoc", "REC-1", 20).
		WillReturnRows(mock.NewRows([]string{"actor_user", "command_name", "created_at", "changes_json"}).
			AddRow("admin", "record.update", "2026-10-28 10:30:00", `[{"field":"title","before":"Old","after":"New"},{"field":"private_note","before":"private-before","after":"private-after"},{"field":"credential","before":"password-before","after":"password-after"}]`))

	handler.HandleRecordHistory(ctx)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, want := range []string{`"actor":"admin"`, `"action":"updated"`, `"created_at":"2026-10-28 10:30:00"`, `"field_label":"Title"`, `"field_type":"Data"`, `"before":"Old"`, `"after":"New"`} {
		if !strings.Contains(body, want) {
			t.Errorf("history response missing %s: %s", want, body)
		}
	}
	for _, forbidden := range []string{"payload_hash", "before_hash", "after_hash", "password-before", "private-before", "Example", `"field":"title"`} {
		if strings.Contains(body, forbidden) {
			t.Errorf("history response exposed %q: %s", forbidden, body)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestHandleRecordHistoryDoesNotQueryAuditWhenRecordIsOutsideReadScope(t *testing.T) {
	handler, registry, mock, database := setupTestHandler(t)
	defer database.Close()
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: "TestDoc", Role: "Sales Rep", Read: true, IfOwner: true})
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/v1/resource/TestDoc/REC-1/history", nil)
	ctx.Params = gin.Params{{Key: "doctype", Value: "TestDoc"}, {Key: "name", Value: "REC-1"}}
	ctx.Set("site_name", "test.local")
	execution, err := policy.NewVerifiedExecution("test-tenant", contract.ActorContext{
		PrincipalID: "reader", SubjectUserID: "reader", PrincipalType: contract.PrincipalHuman,
		Site: "test.local", Roles: []string{"Sales Rep"}, AuthenticatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx.Set("policy_execution", execution)

	mock.ExpectQuery("SELECT title, name, owner, creation, modified, modified_by, doc_status, revision FROM `tabTestDoc` WHERE name = \\? AND owner = \\?").
		WithArgs("REC-1", "reader").
		WillReturnRows(mock.NewRows([]string{"title", "name", "owner", "creation", "modified", "modified_by", "doc_status", "revision"}))

	handler.HandleRecordHistory(ctx)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s, want 404", recorder.Code, recorder.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
