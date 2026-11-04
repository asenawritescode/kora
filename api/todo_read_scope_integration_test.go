//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/schema"
	"github.com/gin-gonic/gin"
)

func TestTodoAssignmentReadScopeHTTP(t *testing.T) {
	handler, database, registry, site := newResourceIntegrationHandler(t)

	todoYAML, err := os.ReadFile("../config/v0/todo/doctypes/todo.yaml")
	if err != nil {
		t.Fatal("read Todo config:", err)
	}
	todoType, err := doctype.ParseYAML(todoYAML, "todo.yaml")
	if err != nil {
		t.Fatal("parse Todo config:", err)
	}
	registry.Register(todoType)
	permissionsYAML, err := os.ReadFile("../config/v0/todo/permissions.yaml")
	if err != nil {
		t.Fatal("read Todo permissions:", err)
	}
	permissions, err := doctype.ParsePermissionsYAML(permissionsYAML)
	if err != nil {
		t.Fatal("parse Todo permissions:", err)
	}
	for _, permission := range permissions {
		registry.Permissions.SetPermission(permission)
	}
	if err := schema.MigrateSiteFromRegistry(database, site, registry, kdb.Resolve("mysql")); err != nil {
		t.Fatal("migrate Todo table:", err)
	}

	for _, task := range []struct{ name, owner, title, assignee string }{
		{"TASK-REP1", "manager@example.test", "Rep One assigned", "rep-one@example.test"},
		{"TASK-REP2", "manager@example.test", "Rep Two assigned", "rep-two@example.test"},
		{"TASK-REP1-OWN", "rep-one@example.test", "Rep One owned", ""},
		{"TASK-REP2-OWN", "rep-two@example.test", "Rep Two owned", ""},
		{"TASK-UNASSIGNED", "manager@example.test", "Unassigned task", ""},
	} {
		if _, err := database.Exec("INSERT INTO `tabTodo` (name, owner, title, assignee) VALUES (?, ?, ?, ?)", task.name, task.owner, task.title, task.assignee); err != nil {
			t.Fatalf("insert task %s fixture: %v", task.name, err)
		}
	}

	request := func(user, role, route string, list bool) *httptest.ResponseRecorder {
		t.Helper()
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodGet, route, nil)
		ctx.Params = gin.Params{{Key: "doctype", Value: "Todo"}}
		if !list {
			ctx.Params = append(ctx.Params, gin.Param{Key: "name", Value: strings.TrimPrefix(route, "/api/resource/Todo/")})
		}
		ctx.Set("site_name", site)
		ctx.Set("site_db", database)
		ctx.Set("site_registry", registry)
		ctx.Set("site_db_type", "mysql")
		ctx.Set("user", user)
		ctx.Set("user_role", role)
		ctx.Set("user_roles", []string{role})
		ctx.Set("policy_execution", integrationTestExecution(t, site, user, []string{role}))
		if list {
			handler.HandleList(ctx)
		} else {
			handler.HandleGet(ctx)
		}
		return recorder
	}
	decodeNames := func(response *httptest.ResponseRecorder) map[string]bool {
		t.Helper()
		if response.Code != http.StatusOK {
			t.Fatalf("Todo list status = %d, want 200: %s", response.Code, response.Body.String())
		}
		var payload struct {
			Data []struct {
				Name string `json:"name"`
			} `json:"data"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
			t.Fatal("decode Todo list:", err)
		}
		names := make(map[string]bool, len(payload.Data))
		for _, row := range payload.Data {
			names[row.Name] = true
		}
		return names
	}

	for _, test := range []struct{ user, assigned, owned, other, otherOwned string }{
		{"rep-one@example.test", "TASK-REP1", "TASK-REP1-OWN", "TASK-REP2", "TASK-REP2-OWN"},
		{"rep-two@example.test", "TASK-REP2", "TASK-REP2-OWN", "TASK-REP1", "TASK-REP1-OWN"},
	} {
		names := decodeNames(request(test.user, "Todo User", "/api/resource/Todo", true))
		if !names[test.assigned] || !names[test.owned] || names[test.other] || names[test.otherOwned] || names["TASK-UNASSIGNED"] || len(names) != 2 {
			t.Errorf("Todo User %s list names = %#v, want only assigned %s and owned %s", test.user, names, test.assigned, test.owned)
		}
		for _, allowed := range []string{test.assigned, test.owned} {
			if response := request(test.user, "Todo User", "/api/resource/Todo/"+allowed, false); response.Code != http.StatusOK {
				t.Errorf("Todo User %s allowed task %s status = %d, want 200: %s", test.user, allowed, response.Code, response.Body.String())
			}
		}
		for _, denied := range []string{test.other, test.otherOwned, "TASK-UNASSIGNED"} {
			if response := request(test.user, "Todo User", "/api/resource/Todo/"+denied, false); response.Code != http.StatusNotFound {
				t.Errorf("Todo User %s other task %s status = %d, want 404: %s", test.user, denied, response.Code, response.Body.String())
			}
		}
	}
	managerNames := decodeNames(request("manager@example.test", "Task Manager", "/api/resource/Todo", true))
	if len(managerNames) != 5 || !managerNames["TASK-REP1"] || !managerNames["TASK-REP2"] || !managerNames["TASK-REP1-OWN"] || !managerNames["TASK-REP2-OWN"] || !managerNames["TASK-UNASSIGNED"] {
		t.Errorf("Task Manager list names = %#v, want all five tasks", managerNames)
	}
}
