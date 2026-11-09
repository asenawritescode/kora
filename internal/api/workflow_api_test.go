package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/policy"
	"github.com/asenawritescode/kora/internal/workflow"
	"github.com/gin-gonic/gin"
)

type apiWorkflowSchemas struct{}

func (apiWorkflowSchemas) Registered(contract.SchemaRef) bool                 { return true }
func (apiWorkflowSchemas) Validate(contract.SchemaRef, json.RawMessage) error { return nil }

type apiWorkflowWaits struct{}

func (apiWorkflowWaits) RegisteredCorrelationField(string, string) bool { return true }

type apiWorkflowDefinitionRepository struct {
	published  int
	tenant     contract.TenantID
	actor      contract.PrincipalID
	key        contract.IdempotencyKey
	definition contract.WorkflowDefinition
	previews   []workflow.PreviewRun
}

func (r *apiWorkflowDefinitionRepository) PublishDefinition(_ context.Context, tenant contract.TenantID, definition contract.WorkflowDefinition, actor contract.PrincipalID, key contract.IdempotencyKey, _ time.Time) (contract.DefinitionVersionRef, error) {
	r.published++
	r.tenant, r.actor, r.key, r.definition = tenant, actor, key, definition
	return contract.DefinitionVersionRef{DefinitionID: definition.ID, Version: definition.Version, Digest: "digest"}, nil
}

func (r *apiWorkflowDefinitionRepository) GetDefinition(context.Context, contract.TenantID, contract.DefinitionID, uint32) (contract.WorkflowDefinition, contract.DefinitionVersionRef, error) {
	return r.definition, contract.DefinitionVersionRef{DefinitionID: r.definition.ID, Version: r.definition.Version, Digest: "digest"}, nil
}

func (r *apiWorkflowDefinitionRepository) ListLatestDefinitions(context.Context, contract.TenantID) ([]contract.DefinitionVersionRef, error) {
	if r.definition.ID == "" {
		return []contract.DefinitionVersionRef{}, nil
	}
	return []contract.DefinitionVersionRef{{DefinitionID: r.definition.ID, Version: r.definition.Version, Digest: "digest"}}, nil
}
func (r *apiWorkflowDefinitionRepository) RecordPreview(_ context.Context, _ contract.TenantID, _ contract.PrincipalID, run workflow.PreviewRun) error {
	r.previews = append(r.previews, run)
	return nil
}
func (r *apiWorkflowDefinitionRepository) ListPreviews(context.Context, contract.TenantID, uint16) ([]workflow.PreviewRun, error) {
	return append([]workflow.PreviewRun(nil), r.previews...), nil
}
func (r *apiWorkflowDefinitionRepository) HasSuccessfulPreview(_ context.Context, _ contract.TenantID, id contract.DefinitionID, version uint32, digest string) (bool, error) {
	for _, run := range r.previews {
		if run.DefinitionID == id && run.DefinitionVersion == version && run.DefinitionDigest == digest && run.Status == "succeeded" {
			return true, nil
		}
	}
	return false, nil
}

type apiWorkflowInstanceRepository struct{}

func (apiWorkflowInstanceRepository) StartInstance(context.Context, workflow.StartInstanceInput) (contract.WorkflowInstance, error) {
	return contract.WorkflowInstance{}, nil
}
func (apiWorkflowInstanceRepository) GetInstance(context.Context, contract.TenantID, contract.InstanceID) (contract.WorkflowInstance, error) {
	return contract.WorkflowInstance{}, nil
}

type apiWorkflowAuthorizer struct{}

func (apiWorkflowAuthorizer) CanManageWorkflowDefinitions(context.Context, policy.Execution) (bool, error) {
	return true, nil
}
func (apiWorkflowAuthorizer) CanStartWorkflow(context.Context, policy.Execution, contract.DefinitionID, contract.SubjectRef) (bool, error) {
	return true, nil
}
func (apiWorkflowAuthorizer) CanReadWorkflow(context.Context, policy.Execution, contract.SubjectRef) (bool, error) {
	return true, nil
}
func (apiWorkflowAuthorizer) CanReconcileWorkflow(context.Context, policy.Execution, contract.WorkflowInstance) (bool, error) {
	return true, nil
}

func TestWorkflowRoutesRequireVerifiedTenantIdentityAndAssembledRuntime(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterWorkflowRoutes(router.Group("/api/v2"), nil)
	unauthenticated := httptest.NewRecorder()
	router.ServeHTTP(unauthenticated, httptest.NewRequest(http.MethodPost, "/api/v2/workflow-definitions/validate", nil))
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated workflow validation status=%d, want 401", unauthenticated.Code)
	}
	execution, err := policy.NewVerifiedExecution("tenant-a", contract.ActorContext{PrincipalID: "operator-a", PrincipalType: contract.PrincipalHuman, Site: "tenant.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	guarded := gin.New()
	guarded.Use(func(c *gin.Context) {
		c.Set("policy_execution", execution)
		c.Set("site_name", "tenant.example.test")
		c.Set("site_id", "tenant-a")
		c.Next()
	})
	RegisterWorkflowRoutes(guarded.Group("/api/v2"), nil)
	unavailable := httptest.NewRecorder()
	guarded.ServeHTTP(unavailable, httptest.NewRequest(http.MethodPost, "/api/v2/workflow-definitions/validate", nil))
	if unavailable.Code != http.StatusServiceUnavailable {
		t.Fatalf("unassembled workflow route status=%d, want 503", unavailable.Code)
	}
}

func TestApprovalPolicyCapabilitiesAndValidationAreTableScoped(t *testing.T) {
	definition := contract.WorkflowDefinition{
		Trigger: &contract.RecordTrigger{Kind: "record.created", Table: "Purchase Request"},
		Steps: []contract.StepDefinition{{ID: "approve", Kind: contract.StepHuman, Human: &contract.HumanSpec{
			Request: contract.HumanRequestTemplate{PolicyID: "purchase-approval-v1"},
		}}},
	}
	policies := map[string][]string{"Purchase Request": {"purchase-approval-v1"}}
	if issues := approvalPolicyIssues(definition, policies); len(issues) != 0 {
		t.Fatalf("registered table policy produced validation issues: %+v", issues)
	}
	definition.Trigger.Table = "Task"
	issues := approvalPolicyIssues(definition, policies)
	if len(issues) != 1 || issues[0].Path != "steps[0].human.request.policy_id" || issues[0].Code != "unsupported_approval_policy" {
		t.Fatalf("policy registered for another table was accepted: %+v", issues)
	}
}

func TestWorkflowDefinitionPublishUsesHeaderAndVerifiedTenantActor(t *testing.T) {
	gin.SetMode(gin.TestMode)
	execution, err := policy.NewVerifiedExecution("stable-tenant-1", contract.ActorContext{PrincipalID: "operator-a", PrincipalType: contract.PrincipalHuman, Site: "tenant.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	catalog := workflow.NewCatalog()
	if err := catalog.Freeze(); err != nil {
		t.Fatal(err)
	}
	repository := &apiWorkflowDefinitionRepository{}
	definitions, err := workflow.NewService(repository, apiWorkflowAuthorizer{}, catalog, apiWorkflowSchemas{}, apiWorkflowWaits{}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	instances, err := workflow.NewInstanceService(repository, apiWorkflowInstanceRepository{}, apiWorkflowAuthorizer{}, catalog, apiWorkflowSchemas{}, apiWorkflowWaits{}, time.Now, nil)
	if err != nil {
		t.Fatal(err)
	}
	services := NewSiteRuntimeServices()
	services.Replace("tenant.example.test", SiteRuntimeService{WorkflowDefinitions: definitions, WorkflowInstances: instances})
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("policy_execution", execution)
		c.Set("site_name", "tenant.example.test")
		c.Set("site_id", "stable-tenant-1")
		c.Next()
	})
	RegisterWorkflowRoutes(router.Group("/api/v2"), services)

	payload := `{"id":"triage","version":1,"entry":"finish","steps":[{"id":"finish","kind":"end","end":{"outcome":"complete"}}]}`
	request := httptest.NewRequest(http.MethodPost, "/api/v2/workflow-definitions", strings.NewReader(payload))
	request.Header.Set("Idempotency-Key", "publish-triage-1")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("publish status=%d body=%s", response.Code, response.Body.String())
	}
	if repository.published != 1 || repository.tenant != "stable-tenant-1" || repository.actor != "operator-a" || repository.key != "publish-triage-1" {
		t.Fatalf("publish did not use verified request scope and key: %+v", repository)
	}

	response = httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v2/workflow-definitions", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"definition_id":"triage"`) {
		t.Fatalf("list definitions status=%d body=%s", response.Code, response.Body.String())
	}
	response = httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v2/workflow-definitions/triage/1", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"outcome":"complete"`) {
		t.Fatalf("get definition status=%d body=%s", response.Code, response.Body.String())
	}
	previewPayload := `{"definition":{"id":"triage","version":1,"entry":"finish","trigger":{"kind":"record.created","table":"Invoice"},"steps":[{"id":"finish","kind":"end","end":{"outcome":"complete"}}]},"record":{}}`
	response = httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v2/workflow-definitions/preview", strings.NewReader(previewPayload)))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"status":"succeeded"`) || len(repository.previews) != 1 {
		t.Fatalf("workflow preview status=%d persisted=%d body=%s", response.Code, len(repository.previews), response.Body.String())
	}
	response = httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v2/workflow-previews", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"definition_id":"triage"`) {
		t.Fatalf("workflow preview history status=%d body=%s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodPost, "/api/v2/workflow-definitions", strings.NewReader(payload))
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || repository.published != 1 {
		t.Fatalf("publish without idempotency header status=%d persisted=%d body=%s; want 400 and no second publish", response.Code, repository.published, response.Body.String())
	}
}
