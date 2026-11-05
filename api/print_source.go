package api

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/orm"
	"github.com/asenawritescode/kora/print"
)

// sitePrintSource serves print formats and record reads for the active site,
// applying the same rules as the resource GET route.
type sitePrintSource struct{ h *Handler }

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
	return nil
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
	return docToMap(doc, dt, registry, nil), nil
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
	return out, nil, nil
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
