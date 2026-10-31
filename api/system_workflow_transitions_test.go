package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/policy"
	"github.com/gin-gonic/gin"
)

func TestHandleSystemDoctypeDoesNotExposeWorkflowConfiguration(t *testing.T) {
	gin.SetMode(gin.TestMode)
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mock.ExpectQuery("INFORMATION_SCHEMA\\.COLUMNS").WillReturnRows(sqlmock.NewRows([]string{"TABLE_NAME", "COLUMN_NAME", "DATA_TYPE", "IS_NULLABLE", "COLUMN_DEFAULT"}))
	mock.ExpectQuery("pg_indexes").WillReturnRows(sqlmock.NewRows([]string{"tablename", "indexname", "indexdef"}))

	registry := doctype.NewRegistry()
	registry.Register(&doctype.DocType{Name: "Activity", Fields: []doctype.Field{{Fieldname: "status", Fieldtype: "Data"}}})
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: "Activity", Role: "Sales Rep", Read: true})
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: "Activity", Role: "Sales Manager", Read: true})
	execution, err := policy.NewVerifiedExecution("tenant-stable", contract.ActorContext{
		PrincipalID: "rep-manager", PrincipalType: contract.PrincipalHuman, SubjectUserID: "rep-manager",
		Site: "acme", Roles: []string{"Sales Rep", "Sales Manager"}, AuthenticatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}

	handler := NewHandler(registry, nil)
	router := gin.New()
	router.GET("/api/system/doctype/:doctype", func(c *gin.Context) {
		c.Set("site_db", database)
		c.Set("site_registry", registry)
		c.Set("site_name", "acme")
		c.Set("site_id", "tenant-stable")
		c.Set("site_db_type", "postgres")
		c.Set("policy_execution", execution)
		handler.HandleSystemDoctype(c)
	})
	request := httptest.NewRequest(http.MethodGet, "/api/system/doctype/Activity?state=Completed", nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var body struct {
		Data SystemDoctypeResponse `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(responseBodyField(t, response.Body.Bytes(), "workflow")) != 0 || len(responseBodyField(t, response.Body.Bytes(), "transitions")) != 0 {
		t.Fatal("doctype response still exposes workflow configuration")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("schema inspection expectations: %v", err)
	}
}

func responseBodyField(t *testing.T, body []byte, field string) json.RawMessage {
	t.Helper()
	var value map[string]json.RawMessage
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatal(err)
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(value["data"], &data); err != nil {
		t.Fatal(err)
	}
	return data[field]
}
