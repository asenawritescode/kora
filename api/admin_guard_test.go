package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/policy"
	"github.com/gin-gonic/gin"
)

func adminGuardRouter(t *testing.T, roles []string, register func(*gin.RouterGroup, *Handler)) (*gin.Engine, sqlmock.Sqlmock) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	mock.MatchExpectationsInOrder(false)
	reg := doctype.NewRegistry()
	exec, err := policy.NewVerifiedExecution(contract.TenantID("t1"), contract.ActorContext{
		PrincipalID: "u1", PrincipalType: contract.PrincipalHuman, SubjectUserID: "u1",
		Site: "s1", Roles: roles, AuthenticatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("site_db", db)
		c.Set("site_registry", reg)
		c.Set("site_name", "s1")
		c.Set("site_id", "t1")
		c.Set("policy_execution", exec)
	})
	register(r.Group("/api"), NewHandler(reg, nil))
	return r, mock
}

func serve(r *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	return w
}

func TestSystemRoutesRejectNonAdminConfigChanges(t *testing.T) {
	r, _ := adminGuardRouter(t, []string{"Sales User"}, RegisterSystemRoutes)
	cases := []struct{ method, path string }{
		{http.MethodPut, "/api/system/permissions"},
		{http.MethodGet, "/api/system/permissions"},
		{http.MethodPost, "/api/system/doctype"},
		{http.MethodPut, "/api/system/doctype/Invoice"},
		{http.MethodDelete, "/api/system/doctype/Invoice"},
		{http.MethodPost, "/api/system/doctype/validate"},
		{http.MethodPost, "/api/system/config/versions/1/activate"},
		{http.MethodPost, "/api/system/config/versions/1/rollback"},
		{http.MethodPost, "/api/system/config/import"},
		{http.MethodPost, "/api/system/config/drafts"},
		{http.MethodGet, "/api/system/roles"},
		{http.MethodPost, "/api/system/roles"},
		{http.MethodDelete, "/api/system/roles/Sales%20User"},
		{http.MethodGet, "/api/system/audit"},
		{http.MethodPut, "/api/system/settings"},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			w := serve(r, tc.method, tc.path, `{}`)
			if w.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403; body=%s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "permission.admin_required") {
				t.Fatalf("body = %s, want permission.admin_required", w.Body.String())
			}
		})
	}
}

func TestSystemRoutesAllowSchemaReadsForSignedInUsers(t *testing.T) {
	r, _ := adminGuardRouter(t, []string{"Sales User"}, RegisterSystemRoutes)
	for _, path := range []string{"/api/system/navigation", "/api/system/doctypes", "/api/system/graph"} {
		if w := serve(r, http.MethodGet, path, ""); w.Code == http.StatusForbidden {
			t.Fatalf("GET %s status = 403, want schema reads open to signed-in users", path)
		}
	}
}

func TestSystemRoutesAllowAdministratorPermissionSave(t *testing.T) {
	r, mock := adminGuardRouter(t, []string{doctype.AdminRole}, RegisterSystemRoutes)
	mock.ExpectBegin()
	for i := 0; i < 4; i++ {
		mock.ExpectExec(".*").WillReturnResult(sqlmock.NewResult(0, 1))
	}
	mock.ExpectCommit()
	w := serve(r, http.MethodPut, "/api/system/permissions", `[{"doctype":"Invoice","role":"Sales User","read":true}]`)
	if w.Code == http.StatusForbidden {
		t.Fatalf("administrator was rejected: %s", w.Body.String())
	}
}

func TestManagementRoutesRejectNonAdmin(t *testing.T) {
	r, _ := adminGuardRouter(t, []string{"Sales User"}, registerManagementRoutes)
	cases := []struct{ method, path string }{
		{http.MethodGet, "/api/system/page-manifests"},
		{http.MethodPost, "/api/system/page-manifests"},
		{http.MethodPut, "/api/system/page-manifests/home"},
		{http.MethodPut, "/api/system/agents/a1"},
		{http.MethodPost, "/api/system/agents/a1/runs"},
		{http.MethodGet, "/api/system/permissions/catalog"},
		{http.MethodGet, "/api/system/config/versions"},
		{http.MethodGet, "/api/system/config/versions/head/snapshot"},
		{http.MethodGet, "/api/system/config/diff"},
		{http.MethodGet, "/api/system/extension"},
		{http.MethodPost, "/api/system/extension"},
		{http.MethodPost, "/api/system/extension/hook/rotate-secret"},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			if w := serve(r, tc.method, tc.path, `{}`); w.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403; body=%s", w.Code, w.Body.String())
			}
		})
	}
}

func TestManagementRoutesAllowAdministrator(t *testing.T) {
	r, _ := adminGuardRouter(t, []string{doctype.AdminRole}, registerManagementRoutes)
	if w := serve(r, http.MethodGet, "/api/system/agents", ""); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

func TestSystemDoctypeCreateRejectsModuleOwnership(t *testing.T) {
	r, _ := adminGuardRouter(t, []string{doctype.AdminRole}, RegisterSystemRoutes)
	body := `{"name":"Invoice","module":"Accounting","module_owner_id":"finance.billing","fields":[{"fieldname":"title","fieldtype":"Data"}]}`
	w := serve(r, http.MethodPost, "/api/system/doctype", body)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "doctype.module_ownership_immutable") {
		t.Fatalf("status = %d body=%s, want 400 doctype.module_ownership_immutable", w.Code, w.Body.String())
	}
}

func TestPreserveModuleOwnershipKeepsInstalledOwner(t *testing.T) {
	current := &doctype.DocType{Name: "Invoice", ModuleOwnerID: "finance.billing", ModuleOwnershipMode: "owned", ModuleAccess: "published"}
	tests := []struct {
		name string
		edit doctype.DocType
	}{
		{"stripped", doctype.DocType{Name: "Invoice"}},
		{"reassigned", doctype.DocType{Name: "Invoice", ModuleOwnerID: "attacker.module", ModuleOwnershipMode: "shared", ModuleAccess: "private"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			edit := tc.edit
			preserveModuleOwnership(&edit, current)
			if edit.ModuleOwnerID != current.ModuleOwnerID || edit.ModuleOwnershipMode != current.ModuleOwnershipMode || edit.ModuleAccess != current.ModuleAccess {
				t.Fatalf("ownership = %q/%q/%q, want installed %q/%q/%q", edit.ModuleOwnerID, edit.ModuleOwnershipMode, edit.ModuleAccess, current.ModuleOwnerID, current.ModuleOwnershipMode, current.ModuleAccess)
			}
		})
	}
}
