package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/facts"
	"github.com/asenawritescode/kora/internal/metrics"
	"github.com/asenawritescode/kora/internal/tenanttime"
)

// metricsAPI serves the metric catalog, the metric query and the metric drill. Every read goes
// through the caller's grants on the record types the metrics read.
type metricsAPI struct {
	registry *doctype.Registry
	siteDB   *sql.DB
	dialect  db.Dialect
	// defs loads the metric definitions of the request's site.
	defs func(*gin.Context) ([]metrics.Metric, error)
}

// registerMetricsRoutes mounts GET /metrics, GET /metrics/:name, POST /metrics/query, POST /metrics/drill,
// the dashboard routes, which read the same catalog, and the saved Explore reports.
func registerMetricsRoutes(apiGroup *gin.RouterGroup, registry *doctype.Registry, siteDB *sql.DB, dialect db.Dialect) {
	defs := newVersionCache[[]metrics.Metric]()
	dashboardDefs := newVersionCache[[]metrics.Dashboard]()
	api := metricsAPI{registry: registry, siteDB: siteDB, dialect: dialect, defs: func(c *gin.Context) ([]metrics.Metric, error) {
		return cachedConfigValue(c, defs, getSiteDB(c, siteDB), siteDialectFromContext(c, dialect), readMetricDefs)
	}}
	api.mount(apiGroup.Group("/metrics"))
	dashboardsAPI{metricsAPI: api, loadDefs: func(c *gin.Context) ([]metrics.Dashboard, error) {
		return cachedConfigValue(c, dashboardDefs, getSiteDB(c, siteDB), siteDialectFromContext(c, dialect), readDashboardDefs)
	}}.mount(apiGroup.Group("/dashboards"))
	registerExploreRoutes(apiGroup, registry)
	reportsAPI{metrics: api}.mount(apiGroup.Group("/explore/reports"))
}

// versionCache keeps each site's parsed configuration with the version it came from. A request reads only
// the active version number; the value is reparsed when that number changes.
type versionCache[T any] struct {
	mu      sync.Mutex
	entries map[string]versionEntry[T]
}

// versionEntry is one site's parsed value and the active version it was parsed from.
type versionEntry[T any] struct {
	version int64
	value   T
}

// newVersionCache returns an empty cache.
func newVersionCache[T any]() *versionCache[T] {
	return &versionCache[T]{entries: map[string]versionEntry[T]{}}
}

// load returns site's value for version, parsing it with parse only when the cache does not hold that
// version. A failed parse is not cached.
func (c *versionCache[T]) load(site string, version int64, parse func() (T, error)) (T, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[site]; ok && e.version == version {
		return e.value, nil
	}
	value, err := parse()
	if err != nil {
		var zero T
		return zero, err
	}
	c.entries[site] = versionEntry[T]{version: version, value: value}
	return value, nil
}

// cachedConfigValue returns the value read from the site's active configuration version by read, parsed
// once per version through cache.
func cachedConfigValue[T any](c *gin.Context, cache *versionCache[T], database *sql.DB, dialect db.Dialect, read func(*sql.DB, db.Dialect, string, int64) (T, error)) (T, error) {
	var zero T
	if database == nil {
		return zero, nil
	}
	site := c.GetString("site_name")
	version, err := activeConfigVersion(database, dialect, site)
	if err != nil {
		return zero, err
	}
	return cache.load(site, version, func() (T, error) {
		return read(database, dialect, site, version)
	})
}

// activeConfigVersion is the site's active configuration version, zero when it has none.
func activeConfigVersion(database *sql.DB, dialect db.Dialect, site string) (int64, error) {
	var version int64
	err := database.QueryRow(db.Rebind(dialect, `SELECT version FROM _kora_config_version WHERE site = ? AND status = 'Active' ORDER BY version DESC LIMIT 1`), site).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return version, err
}

// mount binds the metric routes under group.
func (m metricsAPI) mount(group *gin.RouterGroup) {
	group.GET("", m.list)
	group.GET("/:name", m.describe)
	group.POST("/query", m.query)
	group.POST("/drill", m.drill)
}

// metricsEnvelope is the response of a metric query: the data and how fresh it is.
type metricsEnvelope struct {
	Data any                  `json:"data"`
	Meta contract.MetricsMeta `json:"meta"`
}

// catalog loads the site's metric definitions from its active configuration and indexes them with
// the site's record types.
func (m metricsAPI) catalog(c *gin.Context) (metrics.Catalog, error) {
	registry := analyticsRegistry(c, m.registry)
	defs, err := m.defs(c)
	if err != nil {
		return metrics.Catalog{}, err
	}
	var docs []*doctype.DocType
	if registry != nil {
		docs = registry.All()
	}
	return metrics.NewCatalog(defs, docs), nil
}

// readMetricDefs parses the metric definitions of one configuration version of a site. Version zero
// has none.
func readMetricDefs(database *sql.DB, dialect db.Dialect, site string, version int64) ([]metrics.Metric, error) {
	snapshot, err := readConfigSnapshot(database, dialect, site, version)
	if err != nil || snapshot == nil {
		return nil, err
	}
	return decodeConfigItems[metrics.Metric](snapshot.Metrics, "metric")
}

// readDashboardDefs parses the dashboard definitions of one configuration version of a site. Version zero
// has none.
func readDashboardDefs(database *sql.DB, dialect db.Dialect, site string, version int64) ([]metrics.Dashboard, error) {
	snapshot, err := readConfigSnapshot(database, dialect, site, version)
	if err != nil || snapshot == nil {
		return nil, err
	}
	return decodeConfigItems[metrics.Dashboard](snapshot.Dashboards, "dashboard")
}

// readConfigSnapshot loads and parses one configuration version of a site. Version zero has no snapshot.
func readConfigSnapshot(database *sql.DB, dialect db.Dialect, site string, version int64) (*doctype.ConfigSnapshot, error) {
	if version == 0 {
		return nil, nil
	}
	var configJSON string
	if err := database.QueryRow(db.Rebind(dialect, `SELECT config FROM _kora_config_version WHERE site = ? AND version = ?`), site, version).Scan(&configJSON); err != nil {
		return nil, err
	}
	return doctype.ParseConfig(configJSON)
}

// decodeConfigItems decodes the stored JSON documents of one kind from a configuration snapshot.
func decodeConfigItems[T any](raws []json.RawMessage, kind string) ([]T, error) {
	items := make([]T, 0, len(raws))
	for _, raw := range raws {
		var item T
		if err := json.Unmarshal(raw, &item); err != nil {
			return nil, fmt.Errorf("decode %s definition: %w", kind, err)
		}
		items = append(items, item)
	}
	return items, nil
}

// metricScope is the record scope a metric reads under: the caller's read grant on every record type
// it reads. Owner-only grants narrow to the caller's own records; an entity grant narrows to its entity.
// Field-assigned grants cannot read metrics.
func metricScope(c *gin.Context, registry *doctype.Registry, models []string) (metrics.Scope, *permDenial) {
	var scope metrics.Scope
	for _, model := range models {
		row, denial := permScope(c, registry, model, "read")
		if denial != nil {
			return metrics.Scope{}, denial
		}
		if len(row.AssignedFields) > 0 {
			return metrics.Scope{}, &permDenial{http.StatusForbidden, "metrics are not available to field-assigned grants"}
		}
		if row.Owner {
			if row.UserID == "" {
				return metrics.Scope{}, &permDenial{http.StatusForbidden, "owner-only metrics need an authenticated user"}
			}
			scope.Owner = row.UserID
		}
		if row.Business.Kind == contract.ScopeKindLegalEntity {
			scope.LegalEntity = row.Business.LegalEntityID
		}
	}
	return scope, nil
}

// readable reports whether the caller may read a metric: every record type it reads, directly or
// through its components, must be readable.
func (m metricsAPI) readable(c *gin.Context, cat metrics.Catalog, name string) bool {
	ms, err := cat.Closure([]string{name})
	if err != nil {
		return false
	}
	_, denial := metricScope(c, analyticsRegistry(c, m.registry), metrics.Models(ms))
	return denial == nil
}

// list answers GET /metrics with the metrics the caller may read, by name.
func (m metricsAPI) list(c *gin.Context) {
	cat, err := m.catalog(c)
	if err != nil {
		internalError(c, "loading metric definitions", err)
		return
	}
	out := []contract.MetricSummary{}
	for _, name := range slices.Sorted(maps.Keys(cat.Metrics)) {
		if m.readable(c, cat, name) {
			out = append(out, summaryOf(cat.Metrics[name]))
		}
	}
	c.JSON(http.StatusOK, Response{Data: out})
}

// describe answers GET /metrics/:name. A metric the caller cannot read is not found.
func (m metricsAPI) describe(c *gin.Context) {
	cat, err := m.catalog(c)
	if err != nil {
		internalError(c, "loading metric definitions", err)
		return
	}
	name := c.Param("name")
	def, ok := cat.Metrics[name]
	if !ok || !m.readable(c, cat, name) {
		notFoundError(c, "metrics.not_found", fmt.Sprintf("metric %q not found", name), nil)
		return
	}
	out, err := definitionOf(def)
	if err != nil {
		internalError(c, "encoding metric definition", err)
		return
	}
	c.JSON(http.StatusOK, Response{Data: out})
}

// query answers POST /metrics/query. The query is checked before it is authorized or run.
func (m metricsAPI) query(c *gin.Context) {
	var req contract.MetricQueryRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "validation.invalid_json", "Invalid metrics query", nil)
		return
	}
	database := getSiteDB(c, m.siteDB)
	period, q, ok := m.resolveQuery(c, req, database)
	if !ok {
		return
	}
	cat, err := m.catalog(c)
	if err != nil {
		internalError(c, "loading metric definitions", err)
		return
	}
	cat, adHoc, err := withAdHocMeasures(cat, req.Measures)
	if err != nil {
		writeError(c, http.StatusBadRequest, "metrics.query_invalid", err.Error(), nil)
		return
	}
	q.Metrics = append(q.Metrics, adHoc...)
	ms, refusal := m.authorizeQuery(c, cat, &q)
	if refusal != nil {
		writeError(c, refusal.status, refusal.code, refusal.message, nil)
		return
	}
	if database == nil {
		writeError(c, http.StatusServiceUnavailable, "server.store_unavailable", "Metrics not available for this site", nil)
		return
	}
	engine := metrics.Engine{DB: database, Dialect: siteDialectFromContext(c, m.dialect), Catalog: cat}
	ctx := c.Request.Context()
	res, err := engine.Query(ctx, q)
	if err != nil {
		metricsRunError(c, "running metric query", err)
		return
	}
	models := metrics.Models(ms)
	meta, err := queryMeta(ctx, engine, q, models)
	if err != nil {
		metricsRunError(c, "reading query metadata", err)
		return
	}
	meta = periodMeta(meta, period, res)
	out := resultOf(res)
	out.Labels = adHocLabels(cat, adHoc)
	c.JSON(http.StatusOK, metricsEnvelope{Data: out, Meta: meta})
}

// businessClock is the tenant's business clock. Without a database the default clock serves: the query
// then fails with store unavailable, after its shape and authorization are checked.
func (m metricsAPI) businessClock(c *gin.Context, database *sql.DB) (tenanttime.Clock, error) {
	if database == nil {
		return tenanttime.Default(), nil
	}
	return tenanttime.Load(c.Request.Context(), database, siteDialectFromContext(c, m.dialect), c.GetString("site_name"))
}

// resolveQuery turns a query request into a query over its period, on the tenant's business clock. It
// writes the refusal and reports false when the request is malformed.
func (m metricsAPI) resolveQuery(c *gin.Context, req contract.MetricQueryRequest, database *sql.DB) (queryPeriod, metrics.Query, bool) {
	clock, err := m.businessClock(c, database)
	if err != nil {
		internalError(c, "loading tenant business clock", err)
		return queryPeriod{}, metrics.Query{}, false
	}
	period, err := queryPeriodOf(req, clock, time.Now().UTC())
	if err != nil {
		writeError(c, http.StatusBadRequest, "metrics.query_invalid", err.Error(), nil)
		return queryPeriod{}, metrics.Query{}, false
	}
	q, err := queryFromContract(req, period.span)
	if err != nil {
		writeError(c, http.StatusBadRequest, "metrics.query_invalid", err.Error(), nil)
		return queryPeriod{}, metrics.Query{}, false
	}
	return period, q, true
}

// queryPeriod is the business period a query reads: its preset, custom when the request named From and To,
// the inclusive dates it covers, and the tenant clock that resolved them.
type queryPeriod struct {
	preset metrics.Preset
	span   metrics.Span
	clock  tenanttime.Clock
}

// queryPeriodOf resolves the period of a query request. A preset resolves on the tenant clock at now and
// takes no From or To; custom, or a request with no period, reads From and To.
func queryPeriodOf(req contract.MetricQueryRequest, clock tenanttime.Clock, now time.Time) (queryPeriod, error) {
	preset := metrics.PresetCustom
	if req.Period != nil {
		parsed, err := metrics.ParsePreset(req.Period.Preset)
		if err != nil {
			return queryPeriod{}, err
		}
		preset = parsed
	}
	if preset != metrics.PresetCustom {
		if req.From != "" || req.To != "" {
			return queryPeriod{}, fmt.Errorf("period %q takes no from or to", preset)
		}
		span, err := metrics.ResolvePeriod(clock, now, preset, metrics.Span{})
		return queryPeriod{preset: preset, span: span, clock: clock}, err
	}
	from, to, err := parseMetricRange(req.From, req.To)
	if err != nil {
		return queryPeriod{}, err
	}
	return queryPeriod{preset: preset, span: metrics.Span{From: from, To: to}, clock: clock}, nil
}

// periodMeta adds the period, the comparison window, and the tenant's zone and day cutoff to a result's meta.
func periodMeta(meta contract.MetricsMeta, period queryPeriod, res metrics.Result) contract.MetricsMeta {
	meta.Period = contract.MetricsPeriod{Preset: string(period.preset), From: period.span.From.String(), To: period.span.To.String()}
	if res.PreviousSpan != (metrics.Span{}) {
		meta.Previous = &contract.MetricsSpan{From: res.PreviousSpan.From.String(), To: res.PreviousSpan.To.String()}
	}
	meta.TimeZone = period.clock.ZoneName()
	meta.DayEnds = tenanttime.FormatCutoff(period.clock.Cutoff)
	return meta
}

// queryMeta is the freshness and rebuild state a query result carries.
func queryMeta(ctx context.Context, engine metrics.Engine, q metrics.Query, models []string) (contract.MetricsMeta, error) {
	seq, err := engine.SourceSequence(ctx, modelDocs(engine.Catalog, models))
	if err != nil {
		return contract.MetricsMeta{}, fmt.Errorf("reading metric source sequence: %w", err)
	}
	history, err := queryHistory(ctx, engine.DB, engine.Dialect, models)
	if err != nil {
		return contract.MetricsMeta{}, fmt.Errorf("reading metric history progress: %w", err)
	}
	currencies, err := engine.Currencies(ctx, q)
	if err != nil {
		return contract.MetricsMeta{}, fmt.Errorf("reading query currencies: %w", err)
	}
	return contract.MetricsMeta{AsOf: time.Now().UTC().Format(time.RFC3339), Tier: "S", SourceSequence: seq, History: history, Currencies: currencies}, nil
}

// queryHistory reads the rebuild progress of the models a query reads and summarises it.
func queryHistory(ctx context.Context, database *sql.DB, dialect db.Dialect, models []string) (*contract.MetricsHistory, error) {
	progress, err := (&facts.JobStore{DB: database, Dialect: dialect}).Progress(ctx, models)
	if err != nil {
		return nil, err
	}
	return summariseHistory(progress), nil
}

// summariseHistory is building while any model is rebuilding, and its progress is the least of theirs.
// No rebuilding model, or none read, is ready.
func summariseHistory(progress map[string]facts.Progress) *contract.MetricsHistory {
	h := &contract.MetricsHistory{State: "ready", Progress: 1}
	for _, p := range progress {
		if !p.Building {
			continue
		}
		h.State = "building"
		h.Progress = min(h.Progress, p.Fraction)
	}
	return h
}

// queryRefusal is a query the caller may not run or that does not compile, with its response.
type queryRefusal struct {
	status  int
	code    string
	message string
}

// authorizeQuery names the metrics a query reads, applies the caller's record scope to it and checks
// that it compiles. It returns the metrics the query reads, or the refusal to send.
func (m metricsAPI) authorizeQuery(c *gin.Context, cat metrics.Catalog, q *metrics.Query) ([]metrics.Metric, *queryRefusal) {
	ms, err := cat.Closure(q.Metrics)
	if err != nil {
		return nil, &queryRefusal{http.StatusBadRequest, "metrics.query_invalid", err.Error()}
	}
	reg := analyticsRegistry(c, m.registry)
	scope, denial := metricScope(c, reg, metrics.Models(ms))
	if denial != nil {
		return nil, &queryRefusal{denial.status, "metrics.forbidden", denial.message}
	}
	scope.Joinable = m.joinable(c, reg, cat, q.Dimensions)
	q.Scope = scope
	if err := cat.Check(*q); err != nil {
		code, status := "metrics.query_invalid", http.StatusBadRequest
		switch {
		case errors.Is(err, metrics.ErrRangeTooLarge):
			code = "metrics.range_too_large"
		case errors.Is(err, metrics.ErrForbidden):
			code, status = "metrics.forbidden", http.StatusForbidden
		}
		return nil, &queryRefusal{status, code, err.Error()}
	}
	return ms, nil
}

// joinable names the record types a query's dimension paths and labels may join to. It asks only when a
// query groups by something, and reads every record type once.
func (m metricsAPI) joinable(c *gin.Context, reg *doctype.Registry, cat metrics.Catalog, dims []string) map[string]bool {
	if len(dims) == 0 {
		return nil
	}
	out := make(map[string]bool, len(cat.Models))
	for name := range cat.Models {
		if joinableRecordType(c, reg, name) {
			out[name] = true
		}
	}
	return out
}

// drill answers POST /metrics/drill with the record list behind one number: its filters for the record
// list, and one page of its records. The records apply the caller's record scope.
func (m metricsAPI) drill(c *gin.Context) {
	var req contract.MetricDrillRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "validation.invalid_json", "Invalid metrics drill request", nil)
		return
	}
	from, to, err := parseMetricRange(req.From, req.To)
	if err != nil {
		writeError(c, http.StatusBadRequest, "metrics.drill_invalid", err.Error(), nil)
		return
	}
	run, ok := m.resolveDrill(c, req)
	if !ok {
		return
	}
	page, err := run.engine.DrillRecords(c.Request.Context(), metrics.DrillRequest{Metric: run.name, From: from, To: to, Group: req.Group},
		run.scope, run.clock, req.Cursor)
	if err != nil {
		drillError(c, err)
		return
	}
	out, err := drillOf(page, page.ListOpenable && !run.registry.IsPrivateModuleResource(page.Doctype))
	if err != nil {
		internalError(c, "encoding metric drill", err)
		return
	}
	c.JSON(http.StatusOK, Response{Data: out})
}

// drillRun is what one drill reads: the engine over the tenant's database, the metric or measure it names,
// the caller's scope on it, the tenant's business clock and the registry that says which lists are open.
type drillRun struct {
	engine   metrics.Engine
	name     string
	scope    metrics.Scope
	clock    tenanttime.Clock
	registry *doctype.Registry
}

// resolveDrill resolves a drill's metric, the caller's scope on the record types it reads, and the clock. It writes
// the refusal and reports false when the drill may not run.
func (m metricsAPI) resolveDrill(c *gin.Context, req contract.MetricDrillRequest) (drillRun, bool) {
	cat, err := m.catalog(c)
	if err != nil {
		internalError(c, "loading metric definitions", err)
		return drillRun{}, false
	}
	cat, name, err := drillMetric(cat, req)
	if err != nil {
		writeError(c, http.StatusBadRequest, "metrics.drill_invalid", err.Error(), nil)
		return drillRun{}, false
	}
	ms, err := cat.Closure([]string{name})
	if err != nil {
		writeError(c, http.StatusBadRequest, "metrics.drill_invalid", err.Error(), nil)
		return drillRun{}, false
	}
	registry := analyticsRegistry(c, m.registry)
	scope, denial := metricScope(c, registry, metrics.Models(ms))
	if denial != nil {
		writeError(c, denial.status, "metrics.forbidden", denial.message, nil)
		return drillRun{}, false
	}
	scope.Joinable = m.joinable(c, registry, cat, slices.Collect(maps.Keys(req.Group)))
	database := getSiteDB(c, m.siteDB)
	if database == nil {
		writeError(c, http.StatusServiceUnavailable, "server.store_unavailable", "Metrics not available for this site", nil)
		return drillRun{}, false
	}
	dialect := siteDialectFromContext(c, m.dialect)
	clock, err := tenanttime.Load(c.Request.Context(), database, dialect, c.GetString("site_name"))
	if err != nil {
		internalError(c, "loading tenant business clock", err)
		return drillRun{}, false
	}
	engine := metrics.Engine{DB: database, Dialect: dialect, Catalog: cat}
	return drillRun{engine: engine, name: name, scope: scope, clock: clock, registry: registry}, true
}

// drillMetric names the metric a drill reads: the saved metric the request names, or the unsaved measure
// it asks for, added to the catalog. A drill names exactly one of the two.
func drillMetric(cat metrics.Catalog, req contract.MetricDrillRequest) (metrics.Catalog, string, error) {
	switch {
	case req.Metric != "" && req.Measure == nil:
		return cat, req.Metric, nil
	case req.Measure != nil && req.Metric == "":
		out, names, err := withAdHocMeasures(cat, []contract.MetricAdHocMeasure{*req.Measure})
		if err != nil {
			return metrics.Catalog{}, "", err
		}
		return out, names[0], nil
	}
	return metrics.Catalog{}, "", errors.New("a drill names one metric or one measure")
}

// drillError answers a failed drill: a refused drill or group is 422, a forbidden path 403, a bad
// cursor 400, a deadline a timeout, and anything else internal.
func drillError(c *gin.Context, err error) {
	var refusal metrics.DrillRefusal
	switch {
	case errors.Is(err, metrics.ErrForbidden):
		writeError(c, http.StatusForbidden, "metrics.forbidden", err.Error(), nil)
	case errors.Is(err, metrics.ErrBadCursor):
		writeError(c, http.StatusBadRequest, "metrics.drill_invalid", err.Error(), nil)
	case errors.As(err, &refusal):
		writeError(c, http.StatusUnprocessableEntity, "metrics.drill_invalid", err.Error(), nil)
	default:
		metricsRunError(c, "running metric drill", err)
	}
}

// metricsRunError answers a failed metric run: a deadline is a timeout, anything else is internal.
func metricsRunError(c *gin.Context, msg string, err error) {
	if errors.Is(err, context.DeadlineExceeded) {
		writeError(c, http.StatusGatewayTimeout, "metrics.timeout", "Metric query timed out", nil)
		return
	}
	internalError(c, msg, err)
}

// summaryOf is the listed form of a metric.
func summaryOf(def metrics.Metric) contract.MetricSummary {
	return contract.MetricSummary{
		Name: def.Name, Label: def.Label, Type: string(def.Type), Model: def.Model, Format: def.Format,
		Good: def.Good, Description: def.Description, Verified: def.Verified,
	}
}

// definitionOf is the described form of a metric, filters included.
func definitionOf(def metrics.Metric) (contract.MetricDefinition, error) {
	filters, err := filtersOf(def.Filters)
	if err != nil {
		return contract.MetricDefinition{}, err
	}
	out := contract.MetricDefinition{
		Name: def.Name, Label: def.Label, Type: string(def.Type), Model: def.Model, Format: def.Format,
		Good: def.Good, Description: def.Description, Verified: def.Verified, Time: def.Time,
		Numerator: def.Numerator, Denominator: def.Denominator, Formula: def.Formula, Filters: filters,
	}
	if def.Measure != nil {
		out.Measure = &contract.MetricMeasure{Agg: def.Measure.Agg, Field: def.Measure.Field}
	}
	return out, nil
}

// filtersOf encodes metric filters as wire filters. A value that cannot be encoded is an error.
func filtersOf(in []metrics.Filter) ([]contract.MetricFilter, error) {
	out := make([]contract.MetricFilter, 0, len(in))
	for _, f := range in {
		raw, err := json.Marshal(f.Value)
		if err != nil {
			return nil, fmt.Errorf("encode filter on %q: %w", f.Field, err)
		}
		out = append(out, contract.MetricFilter{Field: f.Field, Op: f.Op, Value: raw})
	}
	return out, nil
}

// queryFromContract converts the wire request to a query over span. Scope is set by the caller.
func queryFromContract(req contract.MetricQueryRequest, span metrics.Span) (metrics.Query, error) {
	filters, err := filtersFromContract(req.Filters)
	if err != nil {
		return metrics.Query{}, err
	}
	return metrics.Query{
		Metrics: req.Metrics, Dimensions: req.Dimensions, Filters: filters, From: span.From, To: span.To,
		Grain: metrics.Grain(req.Grain), Compare: metrics.Compare(req.Compare), Top: req.Top,
	}, nil
}

// filtersFromContract decodes wire filters, whose values are JSON scalars or arrays.
func filtersFromContract(in []contract.MetricFilter) ([]metrics.Filter, error) {
	filters := make([]metrics.Filter, 0, len(in))
	for _, f := range in {
		var value any
		if len(f.Value) > 0 {
			if err := json.Unmarshal(f.Value, &value); err != nil {
				return nil, fmt.Errorf("filter on %q has an invalid value: %w", f.Field, err)
			}
		}
		filters = append(filters, metrics.Filter{Field: f.Field, Op: f.Op, Value: value})
	}
	return filters, nil
}

// withAdHocMeasures adds the request's unsaved measures to the catalog and returns their metric names.
func withAdHocMeasures(cat metrics.Catalog, measures []contract.MetricAdHocMeasure) (metrics.Catalog, []string, error) {
	adHoc := make([]metrics.AdHoc, 0, len(measures))
	for _, a := range measures {
		filters, err := filtersFromContract(a.Filters)
		if err != nil {
			return metrics.Catalog{}, nil, err
		}
		adHoc = append(adHoc, metrics.AdHoc{Model: a.RecordType, Agg: a.Agg, Field: a.Field, Filters: filters})
	}
	return cat.WithAdHoc(adHoc)
}

// adHocLabels maps each unsaved measure's name to its label; nil when the query has none.
func adHocLabels(cat metrics.Catalog, names []string) map[string]string {
	if len(names) == 0 {
		return nil
	}
	out := make(map[string]string, len(names))
	for _, name := range names {
		out[name] = cat.Metrics[name].Label
	}
	return out
}

// parseMetricRange reads an inclusive range of business dates written YYYY-MM-DD.
func parseMetricRange(from, to string) (tenanttime.Date, tenanttime.Date, error) {
	start, err := time.Parse(time.DateOnly, from)
	if err != nil {
		return tenanttime.Date{}, tenanttime.Date{}, fmt.Errorf("from %q is not a YYYY-MM-DD date", from)
	}
	end, err := time.Parse(time.DateOnly, to)
	if err != nil {
		return tenanttime.Date{}, tenanttime.Date{}, fmt.Errorf("to %q is not a YYYY-MM-DD date", to)
	}
	return dateOf(start), dateOf(end), nil
}

// dateOf is the business date of a UTC midnight.
func dateOf(t time.Time) tenanttime.Date {
	return tenanttime.Date{Year: t.Year(), Month: t.Month(), Day: t.Day()}
}

// modelDocs returns the record types named by models, in order, skipping any the registry lacks.
func modelDocs(cat metrics.Catalog, models []string) []*doctype.DocType {
	var out []*doctype.DocType
	for _, name := range models {
		if dt := cat.Models[name]; dt != nil {
			out = append(out, dt)
		}
	}
	return out
}

// resultOf encodes a query result. A null value is the JSON null; any other value is a decimal string,
// so money keeps its exact digits.
func resultOf(res metrics.Result) contract.MetricQueryResult {
	return contract.MetricQueryResult{
		Metrics:    append([]string{}, res.Metrics...),
		Dimensions: append([]string{}, res.Dimensions...),
		Rows:       rowsOf(res.Rows),
		Previous:   rowsOf(res.Previous),
	}
}

// rowsOf encodes result rows for the wire.
func rowsOf(rows []metrics.Row) []contract.MetricRow {
	if rows == nil {
		return nil
	}
	out := make([]contract.MetricRow, 0, len(rows))
	for _, r := range rows {
		values := make([]json.RawMessage, 0, len(r.Values))
		for _, v := range r.Values {
			values = append(values, decimalJSON(v))
		}
		out = append(out, contract.MetricRow{Bucket: r.Period.String(), Group: append([]string{}, r.Group...), Label: r.Label, Values: values})
	}
	return out
}

// decimalJSON is a value as JSON: its exact decimal string, or null when it has no value.
func decimalJSON(v decimal.NullDecimal) json.RawMessage {
	if !v.Valid {
		return json.RawMessage("null")
	}
	return json.RawMessage(strconv.Quote(v.Decimal.String()))
}

// drillOf encodes a drill page as wire records and filters.
func drillOf(page metrics.DrillPage, listOpenable bool) (contract.MetricDrillResult, error) {
	filters, err := filtersOf(page.Filters)
	if err != nil {
		return contract.MetricDrillResult{}, err
	}
	rows := make([]contract.MetricDrillRecord, len(page.Records))
	for i, r := range page.Records {
		rows[i] = contract.MetricDrillRecord{Name: r.Name, Title: r.Title, OccurredAt: r.OccurredAt.UTC().Format(time.RFC3339), Value: r.Value.String()}
	}
	return contract.MetricDrillResult{Doctype: page.Doctype, Filters: filters, ListOpenable: listOpenable,
		Total: page.Total.String(), Rows: rows, NextCursor: page.Next}, nil
}
