package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/plugin"
	"github.com/asenawritescode/kora/policy"
	"github.com/gin-gonic/gin"
)

// The envelope is slightly larger than the maximum 1 MiB input field.
const publishedOperationBodyLimit = (1 << 20) + 4096

const offlineIntentBatchLimit = 20

const offlineQueryBodyLimit = 70000

const offlineIntentBodyLimit = offlineIntentBatchLimit * publishedOperationBodyLimit

func decodeOfflineJSONBody(c *gin.Context, maxBytes int64, destination any) error {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		if err == nil {
			return errors.New("request contains multiple JSON values")
		}
		return err
	}
	return nil
}

func (h *Handler) HandleOfflineCapabilitiesV2(c *gin.Context) {
	execution, ok := requestExecution(c)
	if !ok {
		writeError(c, http.StatusUnauthorized, "auth.unauthenticated", "Verified identity is required", nil)
		return
	}
	scope := businessScopeFromRequest(c, execution)
	catalog, err := h.buildApplicationCatalog(c, execution, scope)
	if err != nil {
		writeError(c, http.StatusServiceUnavailable, "module.catalog_unavailable", "Module catalog is unavailable", nil)
		return
	}
	queries := make([]contract.ApplicationCatalogQuery, 0, len(catalog.PublishedQueries))
	for _, query := range catalog.PublishedQueries {
		if query.OfflineEligible {
			queries = append(queries, query)
		}
	}
	operations := make([]contract.ApplicationCatalogOperation, 0, len(catalog.PublishedOperations))
	for _, operation := range catalog.PublishedOperations {
		if operation.OfflineEligible && operation.Availability == contract.OperationAvailable {
			operations = append(operations, operation)
		}
	}
	capabilities := contract.OfflineCapabilities{ProtocolVersion: 2, CatalogRevision: catalog.Revision, MaxBatchSize: offlineIntentBatchLimit, MaxPageSize: 100, SnapshotLifetimeSeconds: 86400, OfflineQueries: queries, OfflineOperations: operations}
	if err := capabilities.Validate(); err != nil {
		writeError(c, http.StatusServiceUnavailable, "module.catalog_invalid", "Offline capabilities are unavailable", nil)
		return
	}
	c.JSON(http.StatusOK, Response{Data: capabilities})
}

func (h *Handler) HandleOfflineIntentsV2(c *gin.Context) {
	execution, ok := requestExecution(c)
	if !ok {
		writeError(c, http.StatusUnauthorized, "auth.unauthenticated", "Verified identity is required", nil)
		return
	}
	var request contract.OfflineIntentsRequest
	if err := decodeOfflineJSONBody(c, offlineIntentBodyLimit, &request); err != nil || len(request.Intents) == 0 || len(request.Intents) > offlineIntentBatchLimit {
		writeError(c, http.StatusBadRequest, "validation.invalid_offline_batch", "Expected 1 to 20 offline intents", nil)
		return
	}
	services := h.runtimeService(c, c.GetString("site_name"))
	if services.LoadModuleSnapshot == nil || services.ExecutePublishedOperation == nil {
		writeError(c, http.StatusServiceUnavailable, "module.operation_unavailable", "Published operation executor is unavailable", nil)
		return
	}
	snapshot, err := services.LoadModuleSnapshot()
	if err != nil {
		writeError(c, http.StatusServiceUnavailable, "module.catalog_unavailable", "Module catalog is unavailable", nil)
		return
	}
	seen := make(map[string]bool, len(request.Intents))
	receipts := make([]contract.OfflineIntentReceipt, 0, len(request.Intents))
	for _, intent := range request.Intents {
		if intent.Validate() != nil || seen[intent.LocalOperationID] || intent.BusinessScope != businessScopeFromRequest(c, execution) {
			writeError(c, http.StatusBadRequest, "validation.invalid_offline_intent", "Offline intent identity or scope is invalid", nil)
			return
		}
		seen[intent.LocalOperationID] = true
		if intent.CatalogRevision != snapshot.Revision {
			writeError(c, http.StatusConflict, "module.stale_catalog", "Offline intent catalog revision is stale", nil)
			return
		}
		_, operation, found := publishedOperationByRef(services, intent.OperationRef)
		if !found || !operation.published.OfflineEligible {
			writeError(c, http.StatusForbidden, "module.operation_online_only", "Published operation is not eligible for offline replay", nil)
			return
		}
		if intent.InputSchema != operation.spec.Input || contract.ValidateResourceBusinessScope(intent.BusinessScope, operation.published.ScopePolicy) != nil {
			writeError(c, http.StatusConflict, "module.stale_intent", "Offline intent schema or scope is no longer valid", nil)
			return
		}
		if _, authErr := authorizePublishedOperation(execution, h.siteRegistry(c), operation.owner, operation.published, intent.BusinessScope); authErr != nil {
			writeError(c, http.StatusForbidden, "permission.denied", "Published operation access is not granted", nil)
			return
		}
		meta := contract.CommandMeta{IdempotencyKey: intent.IdempotencyKey, ExpectedRevision: intent.ExpectedRevision}
		receipt, executeErr := services.ExecutePublishedOperation(c.Request.Context(), execution, intent.BusinessScope, operation.owner, operation.published, intent.InputSchema, meta, intent.Input)
		if executeErr != nil {
			operationErrorResponse(c, executeErr)
			return
		}
		if receipt.Revision == "" {
			receipt.Revision = strconv.FormatUint(snapshot.Revision, 10)
		}
		if validatePublishedOperationReceipt(receipt, operation, intent.BusinessScope) != nil {
			writeError(c, http.StatusInternalServerError, "module.invalid_operation_receipt", "Published operation returned an invalid receipt", nil)
			return
		}
		receipts = append(receipts, contract.OfflineIntentReceipt{LocalOperationID: intent.LocalOperationID, Receipt: receipt})
	}
	c.JSON(http.StatusOK, Response{Data: contract.OfflineIntentsResponse{Receipts: receipts}})
}

func (h *Handler) HandleOfflineBootstrapV2(c *gin.Context) {
	execution, ok := requestExecution(c)
	if !ok {
		writeError(c, http.StatusUnauthorized, "auth.unauthenticated", "Verified identity is required", nil)
		return
	}
	var request contract.OfflineBootstrapRequest
	if err := decodeOfflineJSONBody(c, offlineQueryBodyLimit, &request); err != nil || request.BusinessScope != businessScopeFromRequest(c, execution) || request.CatalogRevision == 0 || request.Source.InputSchema.ID == "" || len(request.Source.Input) == 0 || request.Limit == 0 || request.Limit > 100 {
		writeError(c, http.StatusBadRequest, "validation.invalid_offline_bootstrap", "Offline bootstrap request is invalid", nil)
		return
	}
	services := h.runtimeService(c, c.GetString("site_name"))
	if services.LoadModuleSnapshot == nil {
		writeError(c, http.StatusServiceUnavailable, "module.catalog_unavailable", "Module catalog is unavailable", nil)
		return
	}
	snapshot, err := services.LoadModuleSnapshot()
	if err != nil || request.CatalogRevision != snapshot.Revision {
		writeError(c, http.StatusConflict, "module.stale_catalog", "Offline bootstrap catalog revision is stale", nil)
		return
	}
	if services.ResolveModuleBindings == nil {
		writeError(c, http.StatusServiceUnavailable, "module.provider_unavailable", "Published query provider is unavailable", nil)
		return
	}
	plan, err := services.ResolveModuleBindings(request.BusinessScope)
	if err != nil {
		writeError(c, http.StatusServiceUnavailable, "module.provider_unavailable", "Published query provider is unavailable", nil)
		return
	}
	query, found := plan.QueryDefinitions[plugin.PublishedQueryKey{Scope: request.BusinessScope, OwnerModuleID: request.Source.OwnerModuleID, Name: request.Source.QueryName}]
	if !found || !query.OfflineEligible || request.Source.InputSchema != query.InputSchema {
		writeError(c, http.StatusForbidden, "module.query_online_only", "Published query is not eligible for offline sync", nil)
		return
	}
	actorScope, authErr := authorizePublishedQueryActorScope(execution, h.siteRegistry(c), query, request.BusinessScope)
	if authErr != nil {
		writeError(c, http.StatusForbidden, "permission.denied", "Published query access is not granted", nil)
		return
	}
	if !plan.CanReplayPublishedQueryOfflineForActor(request.BusinessScope, query.OwnerModuleID, query.Name, actorScope) {
		writeError(c, http.StatusForbidden, "module.query_online_only", "Published query actor scope is not eligible for offline sync", nil)
		return
	}
	actorScopeValue := plugin.PublishedQueryActorScope{BusinessScope: request.BusinessScope, SubjectID: execution.Actor().SubjectUserID, OwnerOnly: actorScope != nil && actorScope.OwnerOnly, OwnerField: query.OwnerField}
	if actorScopeValue.SubjectID == "" {
		actorScopeValue.SubjectID = execution.Actor().PrincipalID
	}
	if actorScope != nil {
		actorScopeValue.AssignedFields = append([]string(nil), actorScope.AssignedFields...)
	}
	var result contract.QueryResult
	if actorScope == nil {
		result, err = plan.ExecutePublishedQuery(c.Request.Context(), request.BusinessScope, query.OwnerModuleID, query.Name, request.Source.Input, request.PageCursor, request.Limit)
	} else {
		result, err = plan.ExecutePublishedQueryForActor(c.Request.Context(), request.BusinessScope, query.OwnerModuleID, query.Name, actorScopeValue, request.Source.Input, request.PageCursor, request.Limit)
	}
	if err != nil || result.SnapshotToken == "" || result.SourceCursor == "" || result.Freshness != contract.QueryFresh {
		writeError(c, http.StatusServiceUnavailable, "module.offline_snapshot_unavailable", "Published query cannot provide a fresh pinned snapshot", nil)
		return
	}
	hash := sha256.Sum256(request.Source.Input)
	inputHash := hex.EncodeToString(hash[:])
	if request.InputHash != "" && request.InputHash != inputHash || request.SnapshotToken != "" && request.SnapshotToken != result.SnapshotToken {
		writeError(c, http.StatusConflict, "module.stale_snapshot", "Offline snapshot identity changed", nil)
		return
	}
	actor := execution.Actor()
	subject := actor.SubjectUserID
	if subject == "" {
		subject = actor.PrincipalID
	}
	c.JSON(http.StatusOK, Response{Data: contract.OfflineSnapshotPage{Source: request.Source, FactKeyField: query.OfflineFactKey, InputHash: inputHash, BusinessScope: request.BusinessScope, SubjectID: subject, SchemaRevision: query.OutputSchema, CatalogRevision: snapshot.Revision, SnapshotToken: result.SnapshotToken, PageCursor: request.PageCursor, NextPageCursor: result.NextCursor, ReplayPosition: result.SourceCursor, Rows: result.Data, Complete: result.NextCursor == ""}})
}

func (h *Handler) HandleOfflinePullV2(c *gin.Context) {
	execution, ok := requestExecution(c)
	if !ok {
		writeError(c, http.StatusUnauthorized, "auth.unauthenticated", "Verified identity is required", nil)
		return
	}
	var request contract.OfflinePullRequest
	if err := decodeOfflineJSONBody(c, offlineQueryBodyLimit, &request); err != nil || request.BusinessScope != businessScopeFromRequest(c, execution) || request.CatalogRevision == 0 || request.SnapshotToken == "" || request.InputHash == "" || request.ReplayPosition == "" || request.Limit == 0 || request.Limit > 100 {
		writeError(c, http.StatusBadRequest, "validation.invalid_offline_pull", "Offline pull request is invalid", nil)
		return
	}
	services := h.runtimeService(c, c.GetString("site_name"))
	if services.LoadModuleSnapshot == nil {
		writeError(c, http.StatusServiceUnavailable, "module.catalog_unavailable", "Module catalog is unavailable", nil)
		return
	}
	snapshot, err := services.LoadModuleSnapshot()
	if err != nil || request.CatalogRevision != snapshot.Revision {
		writeError(c, http.StatusConflict, "module.stale_catalog", "Offline feed catalog revision is stale", nil)
		return
	}
	if services.ResolveModuleBindings == nil {
		writeError(c, http.StatusServiceUnavailable, "module.provider_unavailable", "Published query provider is unavailable", nil)
		return
	}
	plan, err := services.ResolveModuleBindings(request.BusinessScope)
	if err != nil {
		writeError(c, http.StatusServiceUnavailable, "module.provider_unavailable", "Published query provider is unavailable", nil)
		return
	}
	query, found := plan.QueryDefinitions[plugin.PublishedQueryKey{Scope: request.BusinessScope, OwnerModuleID: request.Source.OwnerModuleID, Name: request.Source.QueryName}]
	if !found || !query.OfflineEligible || request.Source.InputSchema != query.InputSchema {
		writeError(c, http.StatusForbidden, "module.query_online_only", "Published query is not eligible for offline sync", nil)
		return
	}
	actorScope, authErr := authorizePublishedQueryActorScope(execution, h.siteRegistry(c), query, request.BusinessScope)
	if authErr != nil {
		writeError(c, http.StatusForbidden, "permission.denied", "Published query access is not granted", nil)
		return
	}
	if !plan.CanReplayPublishedQueryOfflineForActor(request.BusinessScope, query.OwnerModuleID, query.Name, actorScope) {
		writeError(c, http.StatusForbidden, "module.query_online_only", "Published query actor scope is not eligible for offline sync", nil)
		return
	}
	hash := sha256.Sum256(request.Source.Input)
	if hex.EncodeToString(hash[:]) != request.InputHash {
		writeError(c, http.StatusConflict, "module.stale_snapshot", "Offline source input does not match its pinned hash", nil)
		return
	}
	actorScopeValue := plugin.PublishedQueryActorScope{BusinessScope: request.BusinessScope, SubjectID: execution.Actor().SubjectUserID, OwnerOnly: actorScope != nil && actorScope.OwnerOnly, OwnerField: query.OwnerField}
	if actorScopeValue.SubjectID == "" {
		actorScopeValue.SubjectID = execution.Actor().PrincipalID
	}
	if actorScope != nil {
		actorScopeValue.AssignedFields = append([]string(nil), actorScope.AssignedFields...)
	}
	page, err := plan.ReplayPublishedQueryForActor(c.Request.Context(), request.BusinessScope, query.OwnerModuleID, query.Name, actorScopeValue, request.Source.Input, request.SnapshotToken, request.ReplayPosition, request.ThroughCursor, request.PageCursor, request.Limit)
	if err != nil {
		writeError(c, http.StatusServiceUnavailable, "module.offline_replay_unavailable", "Published query replay is unavailable or revoked", nil)
		return
	}
	actor := execution.Actor()
	subject := actor.SubjectUserID
	if subject == "" {
		subject = actor.PrincipalID
	}
	changes := make([]contract.OfflineChange, 0, len(page.Changes))
	for _, change := range page.Changes {
		changes = append(changes, contract.OfflineChange{FactKey: change.FactKey, Deleted: change.Deleted, Data: change.Data})
	}
	c.JSON(http.StatusOK, Response{Data: contract.OfflinePullResponse{Source: request.Source, InputHash: request.InputHash, BusinessScope: request.BusinessScope, SubjectID: subject, SchemaRevision: query.OutputSchema, CatalogRevision: snapshot.Revision, SnapshotToken: request.SnapshotToken, ReplayPosition: request.ReplayPosition, ThroughCursor: page.ThroughCursor, NextPageCursor: page.NextCursor, Changes: changes, HasMore: page.NextCursor != ""}})
}

func (h *Handler) HandleExecutePublishedOperation(c *gin.Context) {
	execution, valid := requestExecution(c)
	if !valid {
		writeError(c, http.StatusUnauthorized, "auth.unauthenticated", "Verified identity is required", nil)
		return
	}
	scope := businessScopeFromRequest(c, execution)
	operationRef := strings.TrimSpace(c.Param("operation"))
	if operationRef == "" {
		writeError(c, http.StatusNotFound, "module.operation_not_found", "Published operation not found", nil)
		return
	}
	var request contract.ExecutePublishedOperation
	decoder := json.NewDecoder(io.LimitReader(c.Request.Body, publishedOperationBodyLimit+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || request.Validate() != nil {
		writeError(c, http.StatusBadRequest, "validation.invalid_operation_request", "Invalid published operation request", nil)
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		writeError(c, http.StatusBadRequest, "validation.invalid_operation_request", "Invalid published operation request", nil)
		return
	}
	services := h.runtimeService(c, c.GetString("site_name"))
	snapshot, operation, ok := publishedOperationByRef(services, operationRef)
	if !ok {
		writeError(c, http.StatusNotFound, "module.operation_not_found", "Published operation not found", nil)
		return
	}
	if request.InputSchema != operation.spec.Input {
		writeError(c, http.StatusBadRequest, "validation.invalid_operation_schema", "Input schema reference does not match the published operation", nil)
		return
	}
	if contract.ValidateResourceBusinessScope(scope, operation.published.ScopePolicy) != nil {
		writeError(c, http.StatusForbidden, "permission.denied", "Published operation scope is not permitted", nil)
		return
	}
	if _, authErr := authorizePublishedOperation(execution, h.siteRegistry(c), operation.owner, operation.published, scope); authErr != nil {
		writeError(c, http.StatusForbidden, "permission.denied", "Published operation access is not granted", nil)
		return
	}
	if services.ExecutePublishedOperation == nil {
		writeError(c, http.StatusServiceUnavailable, "module.operation_unavailable", "Published operation executor is unavailable", nil)
		return
	}
	receipt, err := services.ExecutePublishedOperation(c.Request.Context(), execution, scope, operation.owner, operation.published, request.InputSchema, request.Meta, request.Input)
	if err != nil {
		operationErrorResponse(c, err)
		return
	}
	if receipt.Revision == "" {
		receipt.Revision = strconv.FormatUint(snapshot.Revision, 10)
	}
	if err := validatePublishedOperationReceipt(receipt, operation, scope); err != nil {
		writeError(c, http.StatusInternalServerError, "module.invalid_operation_receipt", "Published operation returned an invalid receipt", nil)
		return
	}
	if writeReceiptValidationError(c, receipt) {
		return
	}
	c.JSON(http.StatusOK, Response{Data: receipt})
}

var schemaFieldPattern = regexp.MustCompile(`additional properties '([^']+)'|at '/?([^']+)'`)

// writeReceiptValidationError reports a stored schema-validation rejection as
// a 400 naming the offending field. The receipt stays the idempotency record,
// so a replay of the same key yields the same response.
func writeReceiptValidationError(c *gin.Context, receipt contract.OperationReceipt) bool {
	if receipt.Status != contract.ReceiptRejected || receipt.Error == nil || receipt.Error.Type != contract.CodeValidationFailed {
		return false
	}
	details := map[string]any{"execution_id": receipt.ExecutionID}
	if receipt.Error.Field != "" {
		details["field"] = receipt.Error.Field
	} else if m := schemaFieldPattern.FindStringSubmatch(receipt.Error.Message); m != nil {
		details["field"] = m[1] + m[2]
	}
	writeError(c, http.StatusBadRequest, string(receipt.Error.Type), receipt.Error.Message, details)
	return true
}

func (h *Handler) HandleLookupPublishedOperation(c *gin.Context) {
	execution, valid := requestExecution(c)
	if !valid {
		writeError(c, http.StatusUnauthorized, "auth.unauthenticated", "Verified identity is required", nil)
		return
	}
	scope := businessScopeFromRequest(c, execution)
	var request contract.LookupPublishedOperation
	decoder := json.NewDecoder(io.LimitReader(c.Request.Body, 4097))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || request.Validate() != nil {
		writeError(c, http.StatusBadRequest, "validation.invalid_operation_lookup", "Invalid published operation lookup", nil)
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		writeError(c, http.StatusBadRequest, "validation.invalid_operation_lookup", "Invalid published operation lookup", nil)
		return
	}
	services := h.runtimeService(c, c.GetString("site_name"))
	_, operation, ok := publishedOperationByRef(services, request.OperationRef)
	if !ok {
		writeError(c, http.StatusNotFound, "module.operation_not_found", "Published operation not found", nil)
		return
	}
	if _, authErr := authorizePublishedOperation(execution, h.siteRegistry(c), operation.owner, operation.published, scope); authErr != nil {
		writeError(c, http.StatusForbidden, "permission.denied", "Published operation access is not granted", nil)
		return
	}
	if contract.ValidateResourceBusinessScope(scope, operation.published.ScopePolicy) != nil {
		writeError(c, http.StatusForbidden, "permission.denied", "Published operation scope is not permitted", nil)
		return
	}
	if services.LookupPublishedOperation == nil {
		writeError(c, http.StatusServiceUnavailable, "module.operation_lookup_unavailable", "Published operation lookup is unavailable", nil)
		return
	}
	receipt, found, err := services.LookupPublishedOperation(c.Request.Context(), execution, scope, operation.owner, operation.published, request.IdempotencyKey)
	if err != nil {
		operationErrorResponse(c, err)
		return
	}
	if !found {
		writeError(c, http.StatusNotFound, "module.operation_receipt_not_found", "No operation receipt exists for this intent", nil)
		return
	}
	if contract.ValidateResourceBusinessScope(scope, operation.published.ScopePolicy) != nil {
		writeError(c, http.StatusForbidden, "permission.denied", "Published operation scope is not permitted", nil)
		return
	}
	if err := validatePublishedOperationReceipt(receipt, operation, scope); err != nil {
		writeError(c, http.StatusInternalServerError, "module.invalid_operation_receipt", "Published operation returned an invalid receipt", nil)
		return
	}
	c.JSON(http.StatusOK, Response{Data: receipt})
}

func (h *Handler) HandleGetPublishedOperationReceipt(c *gin.Context) {
	execution, valid := requestExecution(c)
	if !valid {
		writeError(c, http.StatusUnauthorized, "auth.unauthenticated", "Verified identity is required", nil)
		return
	}
	scope := businessScopeFromRequest(c, execution)
	id, err := contract.ParseOperationID(strings.TrimSpace(c.Param("execution")))
	if err != nil {
		writeError(c, http.StatusNotFound, "module.operation_receipt_not_found", "Operation receipt not found", nil)
		return
	}
	services := h.runtimeService(c, c.GetString("site_name"))
	if services.LookupPublishedOperationByID == nil {
		writeError(c, http.StatusServiceUnavailable, "module.operation_lookup_unavailable", "Published operation lookup is unavailable", nil)
		return
	}
	receipt, found, lookupErr := services.LookupPublishedOperationByID(c.Request.Context(), execution, scope, id)
	if lookupErr != nil {
		operationErrorResponse(c, lookupErr)
		return
	}
	if !found {
		writeError(c, http.StatusNotFound, "module.operation_receipt_not_found", "Operation receipt not found", nil)
		return
	}
	_, operation, ok := publishedOperationByRef(services, receipt.OperationRef)
	if !ok {
		writeError(c, http.StatusNotFound, "module.operation_receipt_not_found", "Operation receipt not found", nil)
		return
	}
	if _, authErr := authorizePublishedOperation(execution, h.siteRegistry(c), operation.owner, operation.published, scope); authErr != nil {
		writeError(c, http.StatusForbidden, "permission.denied", "Operation receipt access is not granted", nil)
		return
	}
	if err := validatePublishedOperationReceipt(receipt, operation, scope); err != nil {
		writeError(c, http.StatusInternalServerError, "module.invalid_operation_receipt", "Published operation returned an invalid receipt", nil)
		return
	}
	c.JSON(http.StatusOK, Response{Data: receipt})
}

func validatePublishedOperationReceipt(receipt contract.OperationReceipt, operation registeredPublishedOperation, scope contract.BusinessScope) error {
	if receipt.Validate() != nil || receipt.BusinessScope != scope || receipt.OperationRef != contract.PublishedOperationRef(operation.owner, operation.published.Capability, operation.published.Operation) {
		return errors.New("receipt identity does not match the published operation")
	}
	if receipt.Status == contract.ReceiptCompleted && (receipt.OutputSchema == nil || *receipt.OutputSchema != operation.spec.Output) {
		return errors.New("receipt output schema does not match the published operation")
	}
	return nil
}

type registeredPublishedOperation struct {
	owner     contract.ModuleID
	published contract.PublishedOperation
	spec      contract.OperationSpec
}

func publishedOperationByRef(services SiteRuntimeService, ref string) (contract.ModuleSnapshot, registeredPublishedOperation, bool) {
	if services.LoadModuleSnapshot == nil {
		return contract.ModuleSnapshot{}, registeredPublishedOperation{}, false
	}
	snapshot, err := services.LoadModuleSnapshot()
	if err != nil {
		return contract.ModuleSnapshot{}, registeredPublishedOperation{}, false
	}
	for _, module := range snapshot.Modules {
		for _, published := range module.PublishedOperations {
			if contract.PublishedOperationRef(module.ID, published.Capability, published.Operation) != ref {
				continue
			}
			for _, offer := range module.Provides {
				if offer.Name == published.Capability {
					for _, spec := range offer.Operations {
						if spec.Name == published.Operation {
							return snapshot, registeredPublishedOperation{module.ID, published, spec}, true
						}
					}
				}
			}
		}
	}
	return snapshot, registeredPublishedOperation{}, false
}

func authorizePublishedOperation(execution policy.Execution, roles policy.RolePolicy, owner contract.ModuleID, published contract.PublishedOperation, scope contract.BusinessScope) (policy.Decision, *contract.Error) {
	if published.ScopePolicy != contract.ScopeTenantShared && published.ScopePolicy != contract.ScopeEntityOwned {
		return policy.Decision{}, contract.NewError(contract.CodePermissionDenied, "published operation scope policy is invalid")
	}
	resource := "module." + string(owner) + ".operation." + published.Name
	return policy.DecideForBusinessScope(execution, roles, resource, "write", scope, published.ScopePolicy == contract.ScopeEntityOwned)
}

func operationErrorResponse(c *gin.Context, err error) {
	var typed *contract.Error
	if !errors.As(err, &typed) || typed == nil {
		writeError(c, http.StatusServiceUnavailable, "module.operation_failed", "Published operation could not be completed", nil)
		return
	}
	status := http.StatusInternalServerError
	switch typed.Type {
	case contract.CodeUnauthenticated:
		status = http.StatusUnauthorized
	case contract.CodePermissionDenied:
		status = http.StatusForbidden
	case contract.CodeValidationFailed:
		status = http.StatusBadRequest
	case contract.CodeNotFound:
		status = http.StatusNotFound
	case contract.CodeConflict, contract.CodeIdempotencyKeyReused, contract.CodeLeaseLost, contract.CodeStaleSubject, contract.CodeOutcomeUnknown:
		status = http.StatusConflict
	case contract.CodeDependencyUnavailable:
		status = http.StatusServiceUnavailable
	case contract.CodeResourceExhausted:
		status = http.StatusTooManyRequests
	}
	var details map[string]any
	if len(typed.Details) > 0 {
		_ = json.Unmarshal(typed.Details, &details)
	}
	if typed.Field != "" {
		if details == nil {
			details = map[string]any{}
		}
		details["field"] = typed.Field
	}
	writeError(c, status, string(typed.Type), typed.Message, details)
}
