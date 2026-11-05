package api

import (
	"errors"
	"strconv"

	"github.com/gin-gonic/gin"

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

// Receipt targets are not supported yet (#143).
func (s sitePrintSource) Receipt(*gin.Context, string, string) (map[string]any, map[string]any, error) {
	return nil, nil, ErrPrintNotFound
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
