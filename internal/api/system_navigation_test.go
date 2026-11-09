package api

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/requests"
	requestsql "github.com/asenawritescode/kora/internal/requests/sqlstore"
	"github.com/asenawritescode/kora/internal/workflow"
	"github.com/gin-gonic/gin"
)

type navTestClock struct{}

func (navTestClock) Now() time.Time { return time.Now() }

type navTestIDs struct{}

func (navTestIDs) NewID() string { return contract.NewID() }

func TestSystemNavigationAdvertisesOnlyAssembledRequestCapability(t *testing.T) {
	database, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	dialect := db.Resolve("mysql")
	store, err := requestsql.New(database, dialect)
	if err != nil {
		t.Fatal(err)
	}
	authorizer, err := requestsql.NewDirectTaskAuthorizer(database, dialect)
	if err != nil {
		t.Fatal(err)
	}
	service, err := requests.NewService(store, authorizer, requests.DirectTaskProjector{}, requests.NoResponseSchemas{}, navTestClock{}, navTestIDs{})
	if err != nil {
		t.Fatal(err)
	}

	gin.SetMode(gin.TestMode)
	router := gin.New()
	handler := NewHandler(doctype.NewRegistry(), nil)
	router.GET("/navigation/:driver/:workflow", func(c *gin.Context) {
		c.Set("site_db_type", c.Param("driver"))
		services := SiteRuntimeService{Requests: service}
		if c.Param("workflow") == "ready" {
			services.WorkflowDefinitions = &workflow.Service{}
			services.WorkflowInstances = &workflow.InstanceService{}
		}
		if c.Param("workflow") == "partial" {
			services.WorkflowDefinitions = &workflow.Service{}
		}
		c.Set("site_runtime_services", services)
		handler.HandleSystemNavigation(c)
	})
	for _, test := range []struct {
		driver        string
		workflowState string
		wantInbox     bool
		wantWorkflow  bool
	}{{driver: "mysql", workflowState: "absent", wantInbox: true}, {driver: "postgres", workflowState: "ready", wantWorkflow: true}, {driver: "mysql", workflowState: "partial", wantInbox: true}} {
		t.Run(test.driver+"-"+test.workflowState, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest("GET", "/navigation/"+test.driver+"/"+test.workflowState, nil))
			if recorder.Code != 200 {
				t.Fatalf("navigation status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			var response struct {
				Data NavigationResponse `json:"data"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if has := containsCapability(response.Data.SupportedCapabilities, "requests.inbox.v2"); has != test.wantInbox {
				t.Fatalf("driver %q inbox capability=%v, want %v", test.driver, has, test.wantInbox)
			}
			if has := containsCapability(response.Data.SupportedCapabilities, "workflow.orchestration.v2"); has != test.wantWorkflow {
				t.Fatalf("workflow %q capability=%v, want %v", test.workflowState, has, test.wantWorkflow)
			}
			if containsCapability(response.Data.SupportedCapabilities, "requests.approvals.v2") {
				t.Fatal("navigation advertised approvals without a domain policy provider")
			}
		})
	}
}

func containsCapability(values []string, name string) bool {
	for _, value := range values {
		if value == name {
			return true
		}
	}
	return false
}

func TestSystemNavigationAdvertisesAgentsOnlyToAdministrators(t *testing.T) {
	for _, test := range []struct {
		roles      []string
		wantAgents bool
	}{{roles: []string{doctype.AdminRole}, wantAgents: true}, {roles: []string{"Sales User"}, wantAgents: false}} {
		router, _ := adminGuardRouter(t, test.roles, func(group *gin.RouterGroup, handler *Handler) {
			group.GET("/system/navigation", handler.HandleSystemNavigation)
		})
		recorder := serve(router, "GET", "/api/system/navigation", "")
		if recorder.Code != 200 {
			t.Fatalf("roles %v: navigation status=%d body=%s", test.roles, recorder.Code, recorder.Body.String())
		}
		var response struct {
			Data NavigationResponse `json:"data"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		hasAgents := false
		for _, capability := range response.Data.AdminCapabilities {
			if capability == "agents" {
				hasAgents = true
			}
		}
		if hasAgents != test.wantAgents {
			t.Fatalf("roles %v: agents capability=%v, want %v (capabilities %v)", test.roles, hasAgents, test.wantAgents, response.Data.AdminCapabilities)
		}
	}
}
