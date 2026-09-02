package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/orm"
	"github.com/gin-gonic/gin"
)

func (h *Handler) HandlePublicList(c *gin.Context) {
	dt, ok := h.publicDocType(c, true)
	if !ok {
		return
	}
	pa := dt.PublicAccess
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", strconv.Itoa(doctype.DefaultPublicMaxLimit)))
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	if limit < 1 {
		limit = doctype.DefaultPublicMaxLimit
	}
	if limit > pa.MaxLimit {
		limit = pa.MaxLimit
	}
	if offset < 0 {
		offset = 0
	}
	filters, err := publicFiltersJSON(pa.Filters, c.Query("filters"), dt)
	if err != nil {
		writeError(c, http.StatusBadRequest, "validation.failed", err.Error(), nil)
		return
	}
	orderBy := pa.SortField + " " + pa.SortOrder
	docs, total, err := h.siteTx(c).GetList(dt, filters, orderBy, limit, offset, "")
	if err != nil {
		internalError(c, "public list query failed", err)
		return
	}
	result := make([]map[string]any, 0, len(docs))
	for _, doc := range docs {
		result = append(result, h.publicDocToMap(c, doc, dt))
	}
	setPublicCacheHeaders(c, pa)
	c.JSON(http.StatusOK, Response{Data: result, Meta: &Meta{DocType: dt.Name, Total: total}})
}

func (h *Handler) HandlePublicGet(c *gin.Context) {
	dt, ok := h.publicDocType(c, false)
	if !ok {
		return
	}
	pa := dt.PublicAccess
	name := c.Param("name")
	doc, err := h.siteTx(c).GetDoc(dt, name, "")
	if err != nil {
		if errors.Is(err, orm.ErrNotFound) {
			writeError(c, http.StatusNotFound, "resource.document_not_found", "Document not found", nil)
			return
		}
		internalError(c, "public get query failed", err)
		return
	}
	if !documentMatchesPublicFilters(doc, pa.Filters) {
		writeError(c, http.StatusNotFound, "resource.document_not_found", "Document not found", nil)
		return
	}
	setPublicCacheHeaders(c, pa)
	c.JSON(http.StatusOK, Response{Data: h.publicDocToMap(c, doc, dt), Meta: &Meta{DocType: dt.Name}})
}

func (h *Handler) publicDocType(c *gin.Context, list bool) (*doctype.DocType, bool) {
	doctypeName := c.Param("doctype")
	dt := h.siteRegistry(c).Get(doctypeName)
	if dt == nil || dt.PublicAccess == nil || !dt.PublicAccess.Enabled {
		writeError(c, http.StatusNotFound, "resource.doctype_not_found", "Document type not found", nil)
		return nil, false
	}
	dt.NormalizePublicAccess()
	if list && !dt.PublicAccess.List {
		writeError(c, http.StatusForbidden, "permission.denied", "Public list is not enabled", nil)
		return nil, false
	}
	if !list && !dt.PublicAccess.Read {
		writeError(c, http.StatusForbidden, "permission.denied", "Public read is not enabled", nil)
		return nil, false
	}
	if err := dt.ValidatePublicAccess(); err != nil {
		writeError(c, http.StatusInternalServerError, "public.access_misconfigured", "Public access is misconfigured", nil)
		return nil, false
	}
	return dt, true
}

func (h *Handler) publicDocToMap(c *gin.Context, doc *doctype.Document, dt *doctype.DocType) map[string]any {
	out := make(map[string]any)
	allowed := dt.PublicFieldSet()
	for field := range allowed {
		switch field {
		case "name":
			out["name"] = doc.Name
		case "doc_status":
			out["doc_status"] = doc.DocStatus
		default:
			if f := dt.GetField(field); f != nil && f.Fieldtype != "Table" {
				value := doc.Get(field)
				out[field] = value
				if shouldExposePublicFileURL(f.Fieldtype) {
					if path, ok := value.(string); ok && strings.TrimSpace(path) != "" {
						key, keyErr := fileKeyForSiteReference(c, path)
						if keyErr != nil {
							continue
						}
						out[field+"_url"] = publicFileURL(c, key)
					}
				}
			}
		}
	}
	return out
}

// publicFileURL keeps the object backend private and avoids exposing an
// internal S3-compatible endpoint in browser-facing API responses.
func publicFileURL(c *gin.Context, key string) string {
	requestPath := c.Request.URL.Path
	prefix := requestPath
	if i := strings.Index(prefix, "/public/resource"); i >= 0 {
		prefix = prefix[:i]
	}
	siteName := c.GetString("site_name")
	if siteName == "" {
		siteName = "default"
	}
	if !strings.HasPrefix(prefix, "/s/") {
		prefix = "/s/" + url.PathEscape(siteName) + prefix
	}
	scheme := c.GetHeader("X-Forwarded-Proto")
	if scheme == "" {
		scheme = "https"
	}
	host := c.GetHeader("X-Forwarded-Host")
	if host == "" {
		host = c.Request.Host
	}
	return (&url.URL{
		Scheme: scheme,
		Host:   host,
		Path:   prefix + "/public/files/" + encodeFileURLPath(key),
	}).String()
}

func encodeFileURLPath(key string) string {
	parts := strings.Split(key, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}

func shouldExposePublicFileURL(fieldtype string) bool {
	switch fieldtype {
	case "Attach", "Attach Image", "Attach Audio":
		return true
	default:
		return false
	}
}

func publicFiltersJSON(serverFilters []doctype.PublicFilter, clientRaw string, dt *doctype.DocType) (string, error) {
	var filters [][]any
	for _, filter := range serverFilters {
		op, value, ok := publicFilterToORM(filter)
		if !ok {
			continue
		}
		filters = append(filters, []any{filter.Field, op, value})
	}
	if strings.TrimSpace(clientRaw) != "" && strings.TrimSpace(clientRaw) != "[]" {
		var clientFilters [][]any
		if err := json.Unmarshal([]byte(clientRaw), &clientFilters); err != nil {
			return "", fmt.Errorf("invalid filters parameter")
		}
		publicFields := dt.PublicFieldSet()
		for _, f := range clientFilters {
			if len(f) != 3 {
				return "", fmt.Errorf("each filter must have field, operator, and value")
			}
			field, ok := f[0].(string)
			if !ok || !publicFields[field] {
				return "", fmt.Errorf("public filters may only use public fields")
			}
			filters = append(filters, f)
		}
	}
	raw, err := json.Marshal(filters)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func publicFilterToORM(filter doctype.PublicFilter) (string, any, bool) {
	switch strings.ToLower(strings.TrimSpace(filter.Op)) {
	case "equals":
		return "=", filter.Value, true
	case "not_equals":
		return "!=", filter.Value, true
	case "in":
		return "in", filter.Value, true
	case "is_set":
		return "is not", nil, true
	case "is_not_set":
		return "is", nil, true
	default:
		return "", nil, false
	}
}

func documentMatchesPublicFilters(doc *doctype.Document, filters []doctype.PublicFilter) bool {
	for _, filter := range filters {
		value := doc.Get(filter.Field)
		switch strings.ToLower(strings.TrimSpace(filter.Op)) {
		case "equals":
			if fmt.Sprint(value) != fmt.Sprint(filter.Value) {
				return false
			}
		case "not_equals":
			if fmt.Sprint(value) == fmt.Sprint(filter.Value) {
				return false
			}
		case "is_set":
			if value == nil || fmt.Sprint(value) == "" {
				return false
			}
		case "is_not_set":
			if value != nil && fmt.Sprint(value) != "" {
				return false
			}
		case "in":
			if !valueInPublicFilter(value, filter.Value) {
				return false
			}
		}
	}
	return true
}

func valueInPublicFilter(value any, expected any) bool {
	items, ok := expected.([]any)
	if !ok {
		return fmt.Sprint(value) == fmt.Sprint(expected)
	}
	for _, item := range items {
		if fmt.Sprint(value) == fmt.Sprint(item) {
			return true
		}
	}
	return false
}

func setPublicCacheHeaders(c *gin.Context, pa *doctype.PublicAccess) {
	maxAge := pa.CacheMaxAge
	if maxAge <= 0 {
		maxAge = doctype.DefaultPublicCacheAge
	}
	c.Header("Cache-Control", fmt.Sprintf("public, max-age=%d", maxAge))
}
