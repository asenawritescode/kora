package workflow

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/policy"
)

type DefinitionRepository interface {
	PublishDefinition(context.Context, contract.TenantID, contract.WorkflowDefinition, contract.PrincipalID, contract.IdempotencyKey, time.Time) (contract.DefinitionVersionRef, error)
	GetDefinition(context.Context, contract.TenantID, contract.DefinitionID, uint32) (contract.WorkflowDefinition, contract.DefinitionVersionRef, error)
}

// DefinitionLister is implemented by durable repositories that can enumerate
// the latest immutable version for each tenant workflow.
type DefinitionLister interface {
	ListLatestDefinitions(context.Context, contract.TenantID) ([]contract.DefinitionVersionRef, error)
}

type DefinitionAuthorizer interface {
	CanManageWorkflowDefinitions(context.Context, policy.Execution) (bool, error)
}

type Service struct {
	repository DefinitionRepository
	authorizer DefinitionAuthorizer
	catalog    *Catalog
	schemas    PayloadSchemaRegistry
	waits      WaitFieldRegistry
	now        func() time.Time
}

func NewService(repository DefinitionRepository, authorizer DefinitionAuthorizer, catalog *Catalog, schemas PayloadSchemaRegistry, waits WaitFieldRegistry, now func() time.Time) (*Service, error) {
	if repository == nil || authorizer == nil || catalog == nil || !catalog.Frozen() || schemas == nil || waits == nil {
		return nil, fmt.Errorf("workflow service requires repository, authorizer, frozen catalog, schemas, and wait registry")
	}
	if err := catalog.ValidateSchemas(schemas); err != nil {
		return nil, err
	}
	if now == nil {
		now = time.Now
	}
	return &Service{repository: repository, authorizer: authorizer, catalog: catalog, schemas: schemas, waits: waits, now: now}, nil
}

func (s *Service) Validate(ctx context.Context, exec policy.Execution, definition contract.WorkflowDefinition) (contract.ValidationResult, error) {
	if s == nil || !exec.Valid() {
		return contract.ValidationResult{}, contract.NewError(contract.CodeUnauthenticated, "verified workflow execution is required")
	}
	allowed, err := s.authorizer.CanManageWorkflowDefinitions(ctx, exec)
	if err != nil {
		return contract.ValidationResult{}, err
	}
	if !allowed {
		return contract.ValidationResult{}, contract.NewError(contract.CodePermissionDenied, "workflow definition access denied")
	}
	result := ValidateDefinition(definition, s.catalog, s.schemas, s.waits)
	if triggerSchemas, ok := s.schemas.(RecordTriggerSchemaRegistry); ok {
		result.Issues = append(result.Issues, ValidateRecordTriggerSchema(definition.Trigger, triggerSchemas)...)
		if definition.Trigger != nil {
			for i, step := range definition.Steps {
				if step.Condition != nil && step.Condition.Field != nil && !triggerSchemas.RecordFieldExists(definition.Trigger.Table, step.Condition.Field.Field) {
					result.Issues = append(result.Issues, contract.ValidationIssue{Path: fmt.Sprintf("steps[%d].condition.field.field", i), Code: "unknown_field", Detail: "condition field does not exist on the selected table"})
				}
			}
		}
		result.Valid = len(result.Issues) == 0
	}
	if fieldTypes, ok := s.schemas.(RecordFieldTypeRegistry); ok {
		result.Issues = append(result.Issues, ValidateRecordActionFields(definition, fieldTypes)...)
		result.Valid = len(result.Issues) == 0
	}
	return result, nil
}

// ValidateCandidate performs the same read-only definition checks used by
// publication, without requiring a user execution identity. Callers must
// already be inside a trusted configuration-validation boundary; this method
// never persists or publishes the definition.
func (s *Service) ValidateCandidate(definition contract.WorkflowDefinition) contract.ValidationResult {
	var recordSchemas RecordTriggerSchemaRegistry
	if schemas, ok := s.schemas.(RecordTriggerSchemaRegistry); ok {
		recordSchemas = schemas
	}
	return s.ValidateCandidateWithRecordSchemas(definition, recordSchemas)
}

// ValidateCandidateWithRecordSchemas validates an inactive configuration
// candidate against its proposed record schema while retaining the tenant's
// registered action and payload schemas. It never persists or publishes.
func (s *Service) ValidateCandidateWithRecordSchemas(definition contract.WorkflowDefinition, recordSchemas RecordTriggerSchemaRegistry) contract.ValidationResult {
	if s == nil || s.catalog == nil || s.schemas == nil {
		return contract.ValidationResult{Valid: false, Issues: []contract.ValidationIssue{{Path: "$", Code: "validator_unavailable", Detail: "workflow validation is unavailable"}}}
	}
	result := ValidateDefinition(definition, s.catalog, s.schemas, s.waits)
	if recordSchemas != nil {
		result.Issues = append(result.Issues, ValidateRecordTriggerSchema(definition.Trigger, recordSchemas)...)
		if definition.Trigger != nil {
			for i, step := range definition.Steps {
				if step.Condition != nil && step.Condition.Field != nil && !recordSchemas.RecordFieldExists(definition.Trigger.Table, step.Condition.Field.Field) {
					result.Issues = append(result.Issues, contract.ValidationIssue{Path: fmt.Sprintf("steps[%d].condition.field.field", i), Code: "unknown_field", Detail: "condition field does not exist on the selected table"})
				}
			}
		}
	}
	fieldTypes, ok := s.schemas.(RecordFieldTypeRegistry)
	if candidateFieldTypes, candidateOK := recordSchemas.(RecordFieldTypeRegistry); candidateOK {
		fieldTypes, ok = candidateFieldTypes, true
	}
	if ok {
		result.Issues = append(result.Issues, ValidateRecordActionFields(definition, fieldTypes)...)
	}
	result.Valid = len(result.Issues) == 0
	return result
}

// Preview validates and simulates a record-triggered definition without
// dispatching actions, then stores a redacted durable result for Studio.
func (s *Service) Preview(ctx context.Context, exec policy.Execution, definition contract.WorkflowDefinition, record json.RawMessage) (PreviewRun, error) {
	if err := s.authorizeManagement(ctx, exec); err != nil {
		return PreviewRun{}, err
	}
	run := PreviewRun{ID: contract.NewID(), DefinitionID: definition.ID, DefinitionVersion: definition.Version, Status: "failed", Steps: []PreviewStep{}, CreatedAt: s.now().UTC()}
	actor, err := contract.ParsePrincipalID(exec.Actor().PrincipalID)
	if err != nil {
		return PreviewRun{}, contract.NewError(contract.CodeUnauthenticated, "workflow preview actor is invalid")
	}
	run.DefinitionDigest, err = DefinitionDigest(definition)
	if err != nil {
		return PreviewRun{}, err
	}
	validation, err := s.Validate(ctx, exec, definition)
	if err == nil && !validation.Valid {
		messages := make([]string, 0, len(validation.Issues))
		for _, issue := range validation.Issues {
			messages = append(messages, issue.Path+": "+issue.Detail)
		}
		err = contract.NewError(contract.CodeValidationFailed, strings.Join(messages, "; "))
	}
	if err == nil {
		var result PreviewResult
		result, err = PreviewRecordDefinition(ctx, definition, record)
		run.Steps, run.Outcome = result.Steps, result.Outcome
	}
	if err == nil {
		run.Status = "succeeded"
	} else {
		run.Failure = err.Error()
		if len(run.Failure) > 2000 {
			run.Failure = run.Failure[:2000]
		}
	}
	repository, ok := s.repository.(PreviewHistoryRepository)
	if !ok {
		return PreviewRun{}, contract.NewError(contract.CodeDependencyUnavailable, "durable workflow preview history is unavailable")
	}
	if persistErr := repository.RecordPreview(ctx, exec.Tenant(), actor, run); persistErr != nil {
		return PreviewRun{}, persistErr
	}
	if err != nil {
		return run, err
	}
	return run, nil
}

func (s *Service) PreviewHistory(ctx context.Context, exec policy.Execution, limit uint16) ([]PreviewRun, error) {
	if err := s.authorizeManagement(ctx, exec); err != nil {
		return nil, err
	}
	repository, ok := s.repository.(PreviewHistoryRepository)
	if !ok {
		return nil, contract.NewError(contract.CodeDependencyUnavailable, "durable workflow preview history is unavailable")
	}
	return repository.ListPreviews(ctx, exec.Tenant(), limit)
}

func (s *Service) Publish(ctx context.Context, exec policy.Execution, definition contract.WorkflowDefinition, key contract.IdempotencyKey) (contract.DefinitionVersionRef, error) {
	if _, err := contract.ParseIdempotencyKey(string(key)); err != nil {
		return contract.DefinitionVersionRef{}, contract.NewError(contract.CodeValidationFailed, "valid idempotency key is required")
	}
	result, err := s.Validate(ctx, exec, definition)
	if err != nil {
		return contract.DefinitionVersionRef{}, err
	}
	if !result.Valid {
		messages := make([]string, 0, len(result.Issues))
		for _, issue := range result.Issues {
			messages = append(messages, issue.Path+": "+issue.Detail)
		}
		return contract.DefinitionVersionRef{}, contract.NewError(contract.CodeValidationFailed, strings.Join(messages, "; "))
	}
	if definition.Trigger != nil && (definition.Trigger.Kind == "record.created" || definition.Trigger.Kind == "record.updated") {
		previews, ok := s.repository.(PreviewHistoryRepository)
		if !ok {
			return contract.DefinitionVersionRef{}, contract.NewError(contract.CodeDependencyUnavailable, "durable workflow preview history is unavailable")
		}
		digest, digestErr := DefinitionDigest(definition)
		if digestErr != nil {
			return contract.DefinitionVersionRef{}, digestErr
		}
		passed, previewErr := previews.HasSuccessfulPreview(ctx, exec.Tenant(), definition.ID, definition.Version, digest)
		if previewErr != nil {
			return contract.DefinitionVersionRef{}, previewErr
		}
		if !passed {
			return contract.DefinitionVersionRef{}, contract.NewError(contract.CodeValidationFailed, "run a successful safe test on this exact workflow version before publishing")
		}
	}
	actor, err := contract.ParsePrincipalID(exec.Actor().PrincipalID)
	if err != nil {
		return contract.DefinitionVersionRef{}, contract.NewError(contract.CodeUnauthenticated, "workflow publisher identity is invalid")
	}
	return s.repository.PublishDefinition(ctx, exec.Tenant(), definition, actor, key, s.now().UTC())
}

// DefinitionDigest identifies the exact definition content used by previews
// and publication readiness checks.
func DefinitionDigest(definition contract.WorkflowDefinition) (string, error) {
	// MySQL JSON columns reorder object keys when persisting RawMessage fields.
	// Canonicalize those embedded values while retaining the stable struct field
	// order for the workflow envelope so the digest survives a database roundtrip.
	encoded, err := json.Marshal(definition)
	if err != nil {
		return "", err
	}
	var canonicalDefinition contract.WorkflowDefinition
	if err := json.Unmarshal(encoded, &canonicalDefinition); err != nil {
		return "", err
	}
	definition = canonicalDefinition
	for index := range definition.Steps {
		step := &definition.Steps[index]
		if step.Action != nil && len(step.Action.Input) > 0 {
			canonical, err := canonicalWorkflowJSON(step.Action.Input)
			if err != nil {
				return "", fmt.Errorf("canonicalize workflow action input: %w", err)
			}
			step.Action.Input = canonical
		}
		if step.Condition != nil && step.Condition.Field != nil && len(step.Condition.Field.Value) > 0 {
			canonical, err := canonicalWorkflowJSON(step.Condition.Field.Value)
			if err != nil {
				return "", fmt.Errorf("canonicalize workflow condition value: %w", err)
			}
			step.Condition.Field.Value = canonical
		}
	}
	body, err := json.Marshal(definition)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:]), nil
}

func canonicalWorkflowJSON(raw json.RawMessage) (json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("trailing JSON value")
		}
		return nil, err
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return canonical, nil
}

func (s *Service) Get(ctx context.Context, exec policy.Execution, id contract.DefinitionID, version uint32) (contract.WorkflowDefinition, contract.DefinitionVersionRef, error) {
	if err := s.authorizeManagement(ctx, exec); err != nil {
		return contract.WorkflowDefinition{}, contract.DefinitionVersionRef{}, err
	}
	return s.repository.GetDefinition(ctx, exec.Tenant(), id, version)
}

func (s *Service) ListLatest(ctx context.Context, exec policy.Execution) ([]contract.DefinitionVersionRef, error) {
	if err := s.authorizeManagement(ctx, exec); err != nil {
		return nil, err
	}
	lister, ok := s.repository.(DefinitionLister)
	if !ok {
		return nil, contract.NewError(contract.CodeDependencyUnavailable, "workflow definition listing is unavailable")
	}
	return lister.ListLatestDefinitions(ctx, exec.Tenant())
}

func (s *Service) AvailableActions() []ActionRegistration {
	if s == nil || s.catalog == nil {
		return nil
	}
	return s.catalog.Registrations()
}

func (s *Service) authorizeManagement(ctx context.Context, exec policy.Execution) error {
	if s == nil || !exec.Valid() {
		return contract.NewError(contract.CodeUnauthenticated, "verified workflow execution is required")
	}
	allowed, err := s.authorizer.CanManageWorkflowDefinitions(ctx, exec)
	if err != nil {
		return err
	}
	if !allowed {
		return contract.NewError(contract.CodePermissionDenied, "workflow definition access denied")
	}
	return nil
}

type InstanceAuthorizer interface {
	CanStartWorkflow(context.Context, policy.Execution, contract.DefinitionID, contract.SubjectRef) (bool, error)
	CanReadWorkflow(context.Context, policy.Execution, contract.SubjectRef) (bool, error)
	CanReconcileWorkflow(context.Context, policy.Execution, contract.WorkflowInstance) (bool, error)
}

// AuthorizationStore is the tenant-persistence boundary used by production
// workflow policies to recheck membership, subject ownership, and start actor.
type AuthorizationStore interface {
	UserRoles(context.Context, string, contract.PrincipalID) ([]string, bool, error)
	Subject(context.Context, contract.SubjectRef) (SubjectAuthorization, error)
	WorkflowStarter(context.Context, contract.TenantID, contract.InstanceID) (contract.PrincipalID, error)
}

type SubjectAuthorization struct {
	Owner      contract.PrincipalID
	Revision   contract.Revision
	UserFields map[string]string
}

type WorkflowCancellationAuthorizer interface {
	CanCancelWorkflow(context.Context, policy.Execution, contract.WorkflowInstance) (bool, error)
}

type InstanceService struct {
	definitions DefinitionRepository
	instances   InstanceRepository
	authorizer  InstanceAuthorizer
	catalog     *Catalog
	schemas     PayloadSchemaRegistry
	waits       WaitFieldRegistry
	now         func() time.Time
	newID       func() string
}

func NewInstanceService(definitions DefinitionRepository, instances InstanceRepository, authorizer InstanceAuthorizer, catalog *Catalog, schemas PayloadSchemaRegistry, waits WaitFieldRegistry, now func() time.Time, newID func() string) (*InstanceService, error) {
	if definitions == nil || instances == nil || authorizer == nil || catalog == nil || !catalog.Frozen() || schemas == nil || waits == nil {
		return nil, fmt.Errorf("workflow instance service dependencies are incomplete")
	}
	if err := catalog.ValidateSchemas(schemas); err != nil {
		return nil, err
	}
	if now == nil {
		now = time.Now
	}
	if newID == nil {
		newID = contract.NewID
	}
	return &InstanceService{definitions: definitions, instances: instances, authorizer: authorizer, catalog: catalog, schemas: schemas, waits: waits, now: now, newID: newID}, nil
}

func (s *InstanceService) Start(ctx context.Context, exec policy.Execution, cmd contract.StartWorkflow) (contract.WorkflowInstance, error) {
	if s == nil || !exec.Valid() {
		return contract.WorkflowInstance{}, contract.NewError(contract.CodeUnauthenticated, "verified workflow execution is required")
	}
	if _, err := contract.ParseIdempotencyKey(string(cmd.Meta.IdempotencyKey)); err != nil {
		return contract.WorkflowInstance{}, contract.NewError(contract.CodeValidationFailed, "valid workflow start idempotency key is required")
	}
	if _, err := contract.ParseDefinitionID(string(cmd.DefinitionID)); err != nil || cmd.DefinitionVersion == 0 {
		return contract.WorkflowInstance{}, contract.NewError(contract.CodeValidationFailed, "valid workflow definition version is required")
	}
	if cmd.Subject.Kind == "" || cmd.Subject.ID == "" {
		return contract.WorkflowInstance{}, contract.NewError(contract.CodeValidationFailed, "workflow subject is required")
	}
	scope := cmd.BusinessScope
	if scope.Kind == "" && scope.TenantID == "" && scope.LegalEntityID == "" {
		scope = contract.BusinessScope{TenantID: exec.Tenant(), Kind: contract.ScopeKindTenant}
	}
	if err := scope.Validate(); err != nil || scope.TenantID != exec.Tenant() {
		return contract.WorkflowInstance{}, contract.NewError(contract.CodePermissionDenied, "workflow business scope is invalid")
	}
	if scope.Kind == contract.ScopeKindLegalEntity {
		if _, ok := exec.GrantForScope(string(cmd.Subject.Kind), "read", scope); !ok {
			return contract.WorkflowInstance{}, contract.NewError(contract.CodePermissionDenied, "workflow subject is not authorized in the requested legal entity")
		}
	}
	if _, err := contract.ParseRevision(string(cmd.Subject.Revision)); err != nil {
		return contract.WorkflowInstance{}, contract.NewError(contract.CodeValidationFailed, "workflow subject revision is invalid")
	}
	allowed, err := s.authorizer.CanStartWorkflow(ctx, exec, cmd.DefinitionID, cmd.Subject)
	if err != nil {
		return contract.WorkflowInstance{}, err
	}
	if !allowed {
		return contract.WorkflowInstance{}, contract.NewError(contract.CodePermissionDenied, "workflow start denied")
	}
	definition, ref, err := s.definitions.GetDefinition(ctx, exec.Tenant(), cmd.DefinitionID, cmd.DefinitionVersion)
	if err != nil {
		return contract.WorkflowInstance{}, err
	}
	if definition.ID != cmd.DefinitionID || definition.Version != cmd.DefinitionVersion || ref.Digest == "" {
		return contract.WorkflowInstance{}, contract.NewError(contract.CodeConflict, "workflow definition version is invalid")
	}
	result := ValidateDefinition(definition, s.catalog, s.schemas, s.waits)
	if !result.Valid {
		return contract.WorkflowInstance{}, contract.NewError(contract.CodeConflict, "published workflow definition failed runtime validation")
	}
	instanceID, err := contract.ParseInstanceID(s.newID())
	if err != nil {
		return contract.WorkflowInstance{}, fmt.Errorf("generate workflow instance ID: %w", err)
	}
	actor, err := contract.ParsePrincipalID(exec.Actor().PrincipalID)
	if err != nil {
		return contract.WorkflowInstance{}, contract.NewError(contract.CodeUnauthenticated, "workflow starter identity is invalid")
	}
	return s.instances.StartInstance(ctx, StartInstanceInput{Tenant: exec.Tenant(), BusinessScope: scope, ID: instanceID, Definition: definition, DefinitionDigest: ref.Digest, Subject: cmd.Subject, Actor: actor, IdempotencyKey: cmd.Meta.IdempotencyKey, Site: exec.Actor().Site, Now: s.now().UTC()})
}

// StartFromRecordEvent starts each published record-triggered definition once.
// It accepts only the Engine kernel's durable record events; unlike the public
// start API, the event actor is already authenticated by the transaction that
// wrote the outbox row. A deterministic idempotency key makes at-least-once
// delivery safe across worker retries and restarts.
func (s *InstanceService) StartFromRecordEvent(ctx context.Context, tenant contract.TenantID, site string, event contract.EventEnvelope) ([]contract.WorkflowInstance, error) {
	if s == nil || ctx == nil || site == "" || event.Source != "kora.kernel" || (event.Type != "record.created" && event.Type != "record.updated") {
		return nil, contract.NewError(contract.CodeValidationFailed, "trusted record trigger event is required")
	}
	if event.Site != site || !event.Actor.Authenticated() || event.Actor.Site != site {
		return nil, contract.NewError(contract.CodeUnauthenticated, "record trigger actor or tenant route is invalid")
	}
	var data contract.RecordChangeEventData
	if err := json.Unmarshal(event.Data, &data); err != nil || data.Table == "" || data.RecordID == "" || data.Revision == "" || event.AggregateType != data.Table || event.AggregateID != data.RecordID {
		return nil, contract.NewError(contract.CodeValidationFailed, "record trigger event data is invalid")
	}
	lister, ok := s.definitions.(DefinitionLister)
	if !ok {
		return nil, contract.NewError(contract.CodeDependencyUnavailable, "workflow definition listing is unavailable")
	}
	refs, err := lister.ListLatestDefinitions(ctx, tenant)
	if err != nil {
		return nil, fmt.Errorf("list record-triggered workflow definitions: %w", err)
	}
	started := make([]contract.WorkflowInstance, 0)
	for _, ref := range refs {
		definition, _, err := s.definitions.GetDefinition(ctx, tenant, ref.DefinitionID, ref.Version)
		if err != nil {
			return nil, fmt.Errorf("load record-triggered workflow %s: %w", ref.DefinitionID, err)
		}
		trigger := definition.Trigger
		if trigger == nil || trigger.Kind != event.Type || trigger.Table != data.Table || !recordTriggerWatches(trigger, data.ChangedFields) {
			continue
		}
		if !ValidateDefinition(definition, s.catalog, s.schemas, s.waits).Valid {
			return nil, contract.NewError(contract.CodeConflict, "published record-triggered workflow failed runtime validation")
		}
		if triggerSchemas, ok := s.schemas.(RecordTriggerSchemaRegistry); ok && len(ValidateRecordTriggerSchema(definition.Trigger, triggerSchemas)) > 0 {
			return nil, contract.NewError(contract.CodeConflict, "published workflow trigger no longer matches the current table schema")
		}
		principal, err := contract.ParsePrincipalID(event.Actor.PrincipalID)
		if err != nil {
			return nil, contract.NewError(contract.CodeUnauthenticated, "record trigger actor identity is invalid")
		}
		keyBytes := sha256.Sum256([]byte(event.ID + "\x00" + string(definition.ID) + "\x00" + fmt.Sprint(definition.Version)))
		key, err := contract.ParseIdempotencyKey("trigger_" + hex.EncodeToString(keyBytes[:]))
		if err != nil {
			return nil, contract.NewError(contract.CodeInternal, "record trigger idempotency key is invalid")
		}
		instanceID, err := contract.ParseInstanceID(s.newID())
		if err != nil {
			return nil, contract.NewError(contract.CodeInternal, "workflow instance identity is invalid")
		}
		businessScope := event.BusinessScope
		if businessScope.Kind == "" && businessScope.TenantID == "" && businessScope.LegalEntityID == "" {
			businessScope = contract.BusinessScope{TenantID: tenant, Kind: contract.ScopeKindTenant}
		}
		if businessScope.Validate() != nil || businessScope.TenantID != tenant {
			return nil, contract.NewError(contract.CodePermissionDenied, "record trigger business scope is invalid")
		}
		startedInstance, err := s.instances.StartInstance(ctx, StartInstanceInput{
			Tenant: tenant, ID: instanceID, Definition: definition, DefinitionDigest: ref.Digest,
			BusinessScope: businessScope, Subject: contract.SubjectRef{Kind: data.Table, ID: data.RecordID, Revision: contract.Revision(data.Revision)},
			Actor: principal, IdempotencyKey: key, Site: site, Now: s.now().UTC(),
		})
		if err != nil {
			return nil, fmt.Errorf("start record-triggered workflow %s: %w", definition.ID, err)
		}
		started = append(started, startedInstance)
	}
	return started, nil
}

func recordTriggerWatches(trigger *contract.RecordTrigger, changed []string) bool {
	if trigger == nil || len(trigger.WatchedFields) == 0 {
		return true
	}
	changedSet := make(map[string]struct{}, len(changed))
	for _, field := range changed {
		changedSet[field] = struct{}{}
	}
	for _, field := range trigger.WatchedFields {
		if _, ok := changedSet[field]; ok {
			return true
		}
	}
	return false
}

func (s *InstanceService) Get(ctx context.Context, exec policy.Execution, id contract.InstanceID) (contract.WorkflowInstance, error) {
	if s == nil || !exec.Valid() {
		return contract.WorkflowInstance{}, contract.NewError(contract.CodeUnauthenticated, "verified workflow execution is required")
	}
	instance, err := s.instances.GetInstance(ctx, exec.Tenant(), id)
	if err != nil {
		return contract.WorkflowInstance{}, err
	}
	allowed, err := s.authorizer.CanReadWorkflow(ctx, exec, instance.Subject)
	if err != nil {
		return contract.WorkflowInstance{}, err
	}
	if !allowed {
		return contract.WorkflowInstance{}, contract.NewError(contract.CodePermissionDenied, "workflow instance access denied")
	}
	return instance, nil
}

// History returns bounded execution history only after the caller is
// authorized to read the associated record and workflow instance.
func (s *InstanceService) History(ctx context.Context, exec policy.Execution, id contract.InstanceID, limit uint16) ([]HistoryEntry, error) {
	if limit == 0 || limit > 200 {
		return nil, contract.NewError(contract.CodeValidationFailed, "workflow history limit must be between 1 and 200")
	}
	if _, err := s.Get(ctx, exec, id); err != nil {
		return nil, err
	}
	repository, ok := s.instances.(HistoryRepository)
	if !ok {
		return nil, contract.NewError(contract.CodeDependencyUnavailable, "workflow history is unavailable")
	}
	return repository.ListHistory(ctx, exec.Tenant(), id, limit)
}

// Recent returns only instances whose record subjects are readable by the
// current actor. The repository query is tenant-scoped and strictly bounded.
func (s *InstanceService) Recent(ctx context.Context, exec policy.Execution, limit uint16) ([]contract.WorkflowInstance, error) {
	if s == nil || !exec.Valid() {
		return nil, contract.NewError(contract.CodeUnauthenticated, "verified workflow execution is required")
	}
	if limit == 0 || limit > 100 {
		return nil, contract.NewError(contract.CodeValidationFailed, "workflow run limit must be between 1 and 100")
	}
	repository, ok := s.instances.(HistoryRepository)
	if !ok {
		return nil, contract.NewError(contract.CodeDependencyUnavailable, "workflow run history is unavailable")
	}
	instances, err := repository.ListRecentInstances(ctx, exec.Tenant(), limit)
	if err != nil {
		return nil, err
	}
	visible := make([]contract.WorkflowInstance, 0, len(instances))
	for _, instance := range instances {
		allowed, authErr := s.authorizer.CanReadWorkflow(ctx, exec, instance.Subject)
		if authErr != nil {
			return nil, authErr
		}
		if allowed {
			visible = append(visible, instance)
		}
	}
	return visible, nil
}

// Cancel cancels a non-running workflow generation after current authorization.
// A live human request must be resolved through the request service so request
// and workflow terminal events can be coordinated by the normal recovery path.
func (s *InstanceService) Cancel(ctx context.Context, exec policy.Execution, id contract.InstanceID, cmd contract.CancelWorkflow) (contract.WorkflowInstance, error) {
	if s == nil || !exec.Valid() {
		return contract.WorkflowInstance{}, contract.NewError(contract.CodeUnauthenticated, "verified workflow execution is required")
	}
	if _, err := contract.ParseInstanceID(string(id)); err != nil {
		return contract.WorkflowInstance{}, contract.NewError(contract.CodeValidationFailed, "workflow instance ID is invalid")
	}
	if _, err := contract.ParseIdempotencyKey(string(cmd.Meta.IdempotencyKey)); err != nil {
		return contract.WorkflowInstance{}, contract.NewError(contract.CodeValidationFailed, "valid workflow cancellation idempotency key is required")
	}
	if _, err := contract.ParseRevision(string(cmd.Meta.ExpectedRevision)); err != nil {
		return contract.WorkflowInstance{}, contract.NewError(contract.CodeValidationFailed, "valid expected workflow revision is required")
	}
	if len(cmd.Reason) == 0 || len(cmd.Reason) > 8192 || strings.TrimSpace(cmd.Reason) != cmd.Reason || strings.TrimSpace(cmd.Reason) == "" {
		return contract.WorkflowInstance{}, contract.NewError(contract.CodeValidationFailed, "workflow cancellation reason is required and bounded")
	}
	record, err := s.instances.GetInstance(ctx, exec.Tenant(), id)
	if err != nil {
		return contract.WorkflowInstance{}, err
	}
	authorizer, ok := s.authorizer.(WorkflowCancellationAuthorizer)
	if !ok {
		return contract.WorkflowInstance{}, contract.NewError(contract.CodePermissionDenied, "workflow cancellation is not authorized")
	}
	allowed, err := authorizer.CanCancelWorkflow(ctx, exec, record)
	if err != nil {
		return contract.WorkflowInstance{}, err
	}
	if !allowed {
		return contract.WorkflowInstance{}, contract.NewError(contract.CodePermissionDenied, "workflow cancellation denied")
	}
	repository, ok := s.instances.(CancellationRepository)
	if !ok {
		return contract.WorkflowInstance{}, contract.NewError(contract.CodeInternal, "workflow repository does not support cancellation")
	}
	actor, err := contract.ParsePrincipalID(exec.Actor().PrincipalID)
	if err != nil {
		return contract.WorkflowInstance{}, contract.NewError(contract.CodeUnauthenticated, "workflow cancellation actor identity is invalid")
	}
	return repository.Cancel(ctx, CancelCommit{Tenant: exec.Tenant(), InstanceID: id, Actor: actor, Site: exec.Actor().Site, ExpectedRevision: cmd.Meta.ExpectedRevision, Reason: cmd.Reason, IdempotencyKey: cmd.Meta.IdempotencyKey})
}

// Reconcile records a privileged operator decision for one uncertain external
// action. It rechecks current authorization and delegates the state transition
// and actor-scoped idempotency receipt to one tenant transaction.
func (s *InstanceService) Reconcile(ctx context.Context, exec policy.Execution, id contract.InstanceID, cmd contract.ReconcileWorkflow) (contract.WorkflowInstance, error) {
	if s == nil || !exec.Valid() {
		return contract.WorkflowInstance{}, contract.NewError(contract.CodeUnauthenticated, "verified workflow execution is required")
	}
	if _, err := contract.ParseInstanceID(string(id)); err != nil {
		return contract.WorkflowInstance{}, contract.NewError(contract.CodeValidationFailed, "workflow instance ID is invalid")
	}
	if _, err := contract.ParseIdempotencyKey(string(cmd.Meta.IdempotencyKey)); err != nil {
		return contract.WorkflowInstance{}, contract.NewError(contract.CodeValidationFailed, "valid reconciliation idempotency key is required")
	}
	if _, err := contract.ParseRevision(string(cmd.Meta.ExpectedRevision)); err != nil {
		return contract.WorkflowInstance{}, contract.NewError(contract.CodeValidationFailed, "valid expected workflow revision is required")
	}
	if _, err := contract.ParseStepID(string(cmd.StepID)); err != nil {
		return contract.WorkflowInstance{}, contract.NewError(contract.CodeValidationFailed, "valid uncertain workflow step is required")
	}
	if len(cmd.EvidenceRef) == 0 || len(cmd.EvidenceRef) > 512 || strings.TrimSpace(cmd.EvidenceRef) != cmd.EvidenceRef || strings.ContainsAny(cmd.EvidenceRef, "\r\n\x00") {
		return contract.WorkflowInstance{}, contract.NewError(contract.CodeValidationFailed, "reconciliation evidence reference is invalid")
	}
	if len(cmd.Reason) == 0 || len(cmd.Reason) > 8192 || strings.TrimSpace(cmd.Reason) == "" {
		return contract.WorkflowInstance{}, contract.NewError(contract.CodeValidationFailed, "reconciliation reason is required and must not exceed 8 KiB")
	}
	switch cmd.Outcome {
	case contract.ReconcileSucceeded, contract.ReconcileNotApplied, contract.ReconcileAbandon:
	default:
		return contract.WorkflowInstance{}, contract.NewError(contract.CodeValidationFailed, "reconciliation outcome is unsupported")
	}
	repository, ok := s.instances.(ReconciliationRepository)
	if !ok {
		return contract.WorkflowInstance{}, contract.NewError(contract.CodeInternal, "workflow repository does not support reconciliation")
	}
	instance, err := s.instances.GetInstance(ctx, exec.Tenant(), id)
	if err != nil {
		return contract.WorkflowInstance{}, err
	}
	allowed, err := s.authorizer.CanReconcileWorkflow(ctx, exec, instance)
	if err != nil {
		return contract.WorkflowInstance{}, err
	}
	if !allowed {
		return contract.WorkflowInstance{}, contract.NewError(contract.CodePermissionDenied, "workflow reconciliation access denied")
	}
	definition, ref, err := s.definitions.GetDefinition(ctx, exec.Tenant(), instance.DefinitionID, instance.DefinitionVersion)
	if err != nil {
		return contract.WorkflowInstance{}, err
	}
	if definition.ID != instance.DefinitionID || definition.Version != instance.DefinitionVersion || ref.DefinitionID != instance.DefinitionID || ref.Version != instance.DefinitionVersion || ref.Digest == "" {
		return contract.WorkflowInstance{}, contract.NewError(contract.CodeConflict, "pinned workflow definition identity is invalid")
	}
	step, found := definitionStepByID(definition, cmd.StepID)
	if !found || step.Kind != contract.StepAction || step.Action == nil {
		return contract.WorkflowInstance{}, contract.NewError(contract.CodeConflict, "reconciliation step is not a pinned action step")
	}
	if _, err := contract.ParseStepID(string(step.Action.Next)); err != nil {
		return contract.WorkflowInstance{}, contract.NewError(contract.CodeConflict, "pinned action destination is invalid")
	}
	status, nextStep := contract.InstanceReady, cmd.StepID
	switch cmd.Outcome {
	case contract.ReconcileSucceeded:
		nextStep = step.Action.Next
	case contract.ReconcileAbandon:
		status = contract.InstanceFailed
	}
	actor, err := contract.ParsePrincipalID(exec.Actor().PrincipalID)
	if err != nil {
		return contract.WorkflowInstance{}, contract.NewError(contract.CodeUnauthenticated, "reconciliation actor identity is invalid")
	}
	return repository.Reconcile(ctx, ReconcileCommit{
		Tenant: exec.Tenant(), InstanceID: id, Actor: actor, ExpectedRevision: cmd.Meta.ExpectedRevision,
		StepID: cmd.StepID, Outcome: cmd.Outcome, EvidenceRef: cmd.EvidenceRef, Reason: cmd.Reason,
		NextStep: nextStep, Status: status, IdempotencyKey: cmd.Meta.IdempotencyKey,
	})
}

func definitionStepByID(definition contract.WorkflowDefinition, id contract.StepID) (contract.StepDefinition, bool) {
	for _, step := range definition.Steps {
		if step.ID == id {
			return step, true
		}
	}
	return contract.StepDefinition{}, false
}
