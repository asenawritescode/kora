package api

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/orm"
	"github.com/asenawritescode/kora/plugin"
	"github.com/asenawritescode/kora/policy"
	"github.com/asenawritescode/kora/print"
)

// sitePrintSource serves print formats and record reads for the active site,
// applying the same rules as the resource GET route.
type sitePrintSource struct{ h *Handler }

// NewSitePrintHandler serves GET print/:format_name for the active site.
func NewSitePrintHandler(h *Handler) gin.HandlerFunc {
	return NewPrintHandler(sitePrintSource{h: h})
}

// NewSitePrintListHandler serves GET print-formats for the active site.
func NewSitePrintListHandler(h *Handler) gin.HandlerFunc {
	return NewPrintListHandler(sitePrintSource{h: h})
}

// NewSitePrintMetaHandler serves GET print-formats/:format_name for the active site.
func NewSitePrintMetaHandler(h *Handler) gin.HandlerFunc {
	return NewPrintMetaHandler(sitePrintSource{h: h})
}

func (s sitePrintSource) Format(c *gin.Context, name string) (*print.Format, error) {
	f, ok := s.h.runtimeService(c, c.GetString("site_name")).PrintFormats[name]
	if !ok {
		return nil, ErrPrintNotFound
	}
	return f, nil
}

// Formats returns the active tenant's print formats.
func (s sitePrintSource) Formats(c *gin.Context) []*print.Format {
	m := s.h.runtimeService(c, c.GetString("site_name")).PrintFormats
	out := make([]*print.Format, 0, len(m))
	for _, f := range m {
		out = append(out, f)
	}
	return out
}

// Authorize mirrors the render route: read on the printed record type, and the
// published-operation grant when the target operation is a published one.
func (s sitePrintSource) Authorize(c *gin.Context, f *print.Format) error {
	execution, ok := requestExecution(c)
	if !ok {
		return ErrPrintForbidden
	}
	registry := s.h.siteRegistry(c)
	for _, dt := range []string{f.For.Record, f.For.Doc} {
		if dt == "" {
			continue
		}
		if _, denial := permScope(c, registry, dt, "read"); denial != nil {
			return ErrPrintForbidden
		}
	}
	if f.For.Operation != "" {
		services := s.h.runtimeService(c, c.GetString("site_name"))
		if _, operation, found := publishedOperationByRef(services, f.For.Operation); found {
			scope := businessScopeFromRequest(c, execution)
			if _, authErr := authorizePublishedOperation(execution, registry, operation.owner, operation.published, scope); authErr != nil {
				return ErrPrintForbidden
			}
		}
	}
	// A format whose queries the actor cannot run would be refused at render,
	// so it must not be offered either (no dead Print button).
	for _, spec := range f.Queries {
		module, name, _ := spec.Ref()
		if s.queryDenied(c, execution, module, name) {
			return ErrPrintForbidden
		}
	}
	return nil
}

// queryDenied reports whether the actor lacks the published query's read
// grant. Anything else (no bindings, unknown query) is left to render.
func (s sitePrintSource) queryDenied(c *gin.Context, execution policy.Execution, module, name string) bool {
	scope := businessScopeFromRequest(c, execution)
	services := s.h.runtimeService(c, c.GetString("site_name"))
	if services.ResolveModuleBindings == nil {
		return false
	}
	plan, err := services.ResolveModuleBindings(scope)
	if err != nil {
		return false
	}
	query, found := plan.QueryDefinitions[plugin.PublishedQueryKey{Scope: scope, OwnerModuleID: contract.ModuleID(module), Name: name}]
	if !found {
		return false
	}
	_, authErr := authorizePublishedQueryActorScope(execution, s.h.siteRegistry(c), query, scope)
	return authErr != nil
}

func (s sitePrintSource) Record(c *gin.Context, dtName, name string) (map[string]any, error) {
	registry := s.h.siteRegistry(c)
	dt := registry.Get(dtName)
	if dt == nil {
		return nil, ErrPrintNotFound
	}
	scope, denial := permScope(c, registry, dt.Name, "read")
	if denial != nil {
		return nil, ErrPrintForbidden
	}
	doc, err := s.h.siteTx(c).GetDocWithScope(dt, name, scope)
	if errors.Is(err, orm.ErrNotFound) {
		return nil, ErrPrintNotFound
	}
	if err != nil {
		return nil, err
	}
	out := docToMap(doc, dt, registry, nil)
	if err := s.printLinkTitles(c, out, dt); err != nil {
		return nil, err
	}
	return out, nil
}

// Receipt returns the completed receipt output of an operation execution the
// actor may see; the endpoint loads .doc separately.
func (s sitePrintSource) Receipt(c *gin.Context, ref, executionID string) (map[string]any, map[string]any, error) {
	execution, ok := requestExecution(c)
	if !ok {
		return nil, nil, ErrPrintForbidden
	}
	id, err := contract.ParseOperationID(strings.TrimSpace(executionID))
	if err != nil {
		return nil, nil, ErrPrintNotFound
	}
	services := s.h.runtimeService(c, c.GetString("site_name"))
	if services.LookupPublishedOperationByID == nil {
		return nil, nil, ErrPrintNotFound
	}
	scope := businessScopeFromRequest(c, execution)
	receipt, found, err := services.LookupPublishedOperationByID(c.Request.Context(), execution, scope, id)
	if err != nil {
		return nil, nil, err
	}
	if !found || receipt.OperationRef != ref || receipt.Status != contract.ReceiptCompleted {
		return nil, nil, ErrPrintNotFound
	}
	_, operation, ok := publishedOperationByRef(services, receipt.OperationRef)
	if !ok {
		return nil, nil, ErrPrintNotFound
	}
	if _, authErr := authorizePublishedOperation(execution, s.h.siteRegistry(c), operation.owner, operation.published, scope); authErr != nil {
		return nil, nil, ErrPrintForbidden
	}
	var out map[string]any
	if err := json.Unmarshal(receipt.Output, &out); err != nil {
		return nil, nil, ErrPrintNotFound
	}
	return unwrapCommandOutput(out), nil, nil
}

// unwrapCommandOutput returns the declared output of a command-backed
// operation. Command receipts are stored as {command, steps, output:{...}};
// templates and print references address the declared output, so the envelope
// is peeled off. Flat outputs from other operation kinds pass through.
func unwrapCommandOutput(out map[string]any) map[string]any {
	if _, isCommand := out["command"]; isCommand {
		if declared, ok := out["output"].(map[string]any); ok {
			return declared
		}
	}
	return out
}

// Tenant reads print.* overrides from the site secret store.
func (s sitePrintSource) Tenant(c *gin.Context) (map[string]any, print.Options) {
	site := c.GetString("site_name")
	name, opt := site, print.Options{Precision: 2}
	if store := s.h.secretStore(c); store != nil {
		if v, err := store.Get(site, "print.tenant_name"); err == nil && v != "" {
			name = v
		}
		if v, err := store.Get(site, "print.currency"); err == nil {
			opt.Currency = v
		}
		if v, err := store.Get(site, "print.precision"); err == nil {
			if n, err := strconv.Atoi(v); err == nil && n >= 0 {
				opt.Precision = n
			}
		}
	}
	return map[string]any{"name": name}, opt
}

// Query runs a published query through the same plan and actor grant as
// HandlePublishedModuleQuery, capped at the query's own page limit.
func (s sitePrintSource) Query(c *gin.Context, module, name string, input map[string]any) ([]map[string]any, error) {
	execution, ok := requestExecution(c)
	if !ok {
		return nil, ErrPrintForbidden
	}
	scope := businessScopeFromRequest(c, execution)
	services := s.h.runtimeService(c, c.GetString("site_name"))
	if services.ResolveModuleBindings == nil {
		return nil, ErrPrintRenderFailed
	}
	plan, err := services.ResolveModuleBindings(scope)
	if err != nil {
		return nil, ErrPrintRenderFailed
	}
	moduleID := contract.ModuleID(module)
	query, found := plan.QueryDefinitions[plugin.PublishedQueryKey{Scope: scope, OwnerModuleID: moduleID, Name: name}]
	if !found {
		return nil, ErrPrintNotFound
	}
	actorScope, authErr := authorizePublishedQueryActorScope(execution, s.h.siteRegistry(c), query, scope)
	if authErr != nil {
		return nil, ErrPrintForbidden
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return nil, ErrPrintRenderFailed
	}
	var result contract.QueryResult
	if actorScope == nil {
		result, err = plan.ExecutePublishedQuery(c.Request.Context(), scope, moduleID, name, raw, "", query.PageLimit)
	} else {
		result, err = plan.ExecutePublishedQueryForActor(c.Request.Context(), scope, moduleID, name, *actorScope, raw, "", query.PageLimit)
	}
	var rows []map[string]any
	if err != nil || json.Unmarshal(result.Data, &rows) != nil {
		return nil, ErrPrintRenderFailed
	}
	return rows, nil
}
