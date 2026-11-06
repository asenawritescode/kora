package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/doctype"
)

func TestRoleDeleteRefusesAdministratorRole(t *testing.T) {
	tests := []struct {
		name string
		role string
	}{
		{"exact", "Administrator"},
		{"lower case", "administrator"},
		{"upper case", "ADMINISTRATOR"},
		{"mixed case", "AdMiNiStRaToR"},
		{"surrounding whitespace", " Administrator "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler, reg, mock, database := setupTestHandler(t)
			defer database.Close()
			gin.SetMode(gin.TestMode)

			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodDelete, "/api/system/roles/x", nil)
			ctx.Params = gin.Params{{Key: "name", Value: tt.role}}
			injectContext(ctx)
			injectDB(ctx, database, reg)

			// No SQL expectations: the guard must reject before touching the database.
			handler.HandleSystemRoleDelete(ctx)

			if recorder.Code != http.StatusConflict {
				t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusConflict, recorder.Body.String())
			}
			var body struct {
				Error ErrorBody `json:"error"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode error body: %v", err)
			}
			if body.Error.Code != "role.protected" {
				t.Fatalf("error code = %q, want role.protected", body.Error.Code)
			}
			if body.Error.Message == "" {
				t.Fatal("error message is empty")
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("unexpected SQL for protected role: %v", err)
			}
		})
	}
}

func TestRoleDeleteHonoursConfiguredAdminRoleName(t *testing.T) {
	previous := doctype.AdminRole
	doctype.SetAdminRole("Root")
	defer doctype.SetAdminRole(previous)

	handler, reg, _, database := setupTestHandler(t)
	defer database.Close()
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodDelete, "/api/system/roles/root", nil)
	ctx.Params = gin.Params{{Key: "name", Value: "root"}}
	injectContext(ctx)
	injectDB(ctx, database, reg)

	handler.HandleSystemRoleDelete(ctx)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusConflict, recorder.Body.String())
	}
}

func TestRoleDeleteStillDeletesOtherRoles(t *testing.T) {
	handler, reg, mock, database := setupTestHandler(t)
	defer database.Close()
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodDelete, "/api/system/roles/Store%20Manager", nil)
	ctx.Params = gin.Params{{Key: "name", Value: "Store Manager"}}
	injectContext(ctx)
	injectDB(ctx, database, reg)
	ctx.Set("site_db_type", "postgres")
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM _kora_user`).
		WithArgs("Store Manager").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM _kora_role WHERE name = \$1`).WithArgs("Store Manager").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`DELETE FROM _kora_permission WHERE role = \$1`).WithArgs("Store Manager").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE _kora_auth_revision`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE _kora_config_revision`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	handler.HandleSystemRoleDelete(ctx)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL expectations: %v", err)
	}
}
