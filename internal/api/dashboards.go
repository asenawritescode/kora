package api

import (
	"cmp"
	"fmt"
	"net/http"
	"slices"

	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/metrics"
)

// dashboardsAPI serves the dashboards of the site's configuration with the tenant's layout override applied.
// A dashboard shows only the tiles and charts the caller may read, so it uses the metric catalog and its
// permission check.
type dashboardsAPI struct {
	metricsAPI
	// loadDefs loads the dashboard definitions of the request's site.
	loadDefs func(*gin.Context) ([]metrics.Dashboard, error)
}

// mount binds the dashboard routes under group.
func (d dashboardsAPI) mount(group *gin.RouterGroup) {
	group.GET("", d.list)
	group.GET("/:name", d.describe)
	group.PUT("/:name/override", d.saveOverride)
	group.DELETE("/:name/override", d.resetOverride)
}

// list answers GET /dashboards with every dashboard the caller can read anything of, by name.
func (d dashboardsAPI) list(c *gin.Context) {
	cat, dashboards, overrides, ok := d.load(c)
	if !ok {
		return
	}
	out := []contract.DashboardDefinition{}
	for _, dash := range slices.SortedFunc(slices.Values(dashboards), func(a, b metrics.Dashboard) int {
		return cmp.Compare(a.Name, b.Name)
	}) {
		if view, visible := d.view(c, cat, dash, overrides); visible {
			out = append(out, view)
		}
	}
	c.JSON(http.StatusOK, Response{Data: out})
}

// describe answers GET /dashboards/:name. A dashboard the caller can read nothing of is not found.
func (d dashboardsAPI) describe(c *gin.Context) {
	cat, dashboards, overrides, ok := d.load(c)
	if !ok {
		return
	}
	name := c.Param("name")
	if dash, found := findDashboard(dashboards, name); found {
		if view, visible := d.view(c, cat, dash, overrides); visible {
			c.JSON(http.StatusOK, Response{Data: view})
			return
		}
	}
	notFoundError(c, "dashboards.not_found", fmt.Sprintf("dashboard %q not found", name), nil)
}

// load reads what every dashboard request needs: the metric catalog, the dashboard definitions and the
// tenant's overrides. It writes the error response and reports false when one of them is unavailable.
func (d dashboardsAPI) load(c *gin.Context) (metrics.Catalog, []metrics.Dashboard, map[string]contract.DashboardOverride, bool) {
	cat, err := d.catalog(c)
	if err != nil {
		internalError(c, "loading metric definitions", err)
		return metrics.Catalog{}, nil, nil, false
	}
	dashboards, err := d.loadDefs(c)
	if err != nil {
		internalError(c, "loading dashboard definitions", err)
		return metrics.Catalog{}, nil, nil, false
	}
	database := getSiteDB(c, d.siteDB)
	if database == nil {
		writeError(c, http.StatusServiceUnavailable, "server.store_unavailable", "Dashboards not available for this site", nil)
		return metrics.Catalog{}, nil, nil, false
	}
	overrides, err := readDashboardOverrides(c.Request.Context(), database)
	if err != nil {
		internalError(c, "reading dashboard overrides", err)
		return metrics.Catalog{}, nil, nil, false
	}
	return cat, dashboards, overrides, true
}

// view is the dashboard as the caller sees it. Tiles and charts the caller cannot read are dropped, and a
// dashboard left with neither is not visible. The tenant's override is applied last.
func (d dashboardsAPI) view(c *gin.Context, cat metrics.Catalog, dash metrics.Dashboard, overrides map[string]contract.DashboardOverride) (contract.DashboardDefinition, bool) {
	tiles := []contract.DashboardTile{}
	for _, t := range dash.Tiles {
		if d.readable(c, cat, t.Metric) {
			tiles = append(tiles, contract.DashboardTile{Metric: t.Metric, Compare: t.Compare})
		}
	}
	charts := []contract.DashboardChart{}
	for _, ch := range dash.Charts {
		if d.readableAll(c, cat, ch.Metrics) {
			charts = append(charts, chartOf(ch))
		}
	}
	if len(tiles) == 0 && len(charts) == 0 {
		return contract.DashboardDefinition{}, false
	}
	def := contract.DashboardDefinition{
		Name: dash.Name, Label: dash.Label, Description: dash.Description, DefaultRange: dash.DefaultRange,
		Tiles: tiles, Charts: charts,
	}
	return applyDashboardOverride(def, overrides[dash.Name]), true
}

// readableAll reports whether the caller may read every named metric.
func (d dashboardsAPI) readableAll(c *gin.Context, cat metrics.Catalog, names []string) bool {
	for _, name := range names {
		if !d.readable(c, cat, name) {
			return false
		}
	}
	return true
}

// chartOf is the wire form of a chart definition.
func chartOf(ch metrics.Chart) contract.DashboardChart {
	return contract.DashboardChart{
		ID: ch.ID, Title: ch.Title, Kind: ch.Kind, Metrics: append([]string{}, ch.Metrics...), By: ch.By,
		Grain: ch.Grain, Top: ch.Top, Compare: ch.Compare, Drill: ch.Drill,
	}
}

// findDashboard returns the dashboard named name.
func findDashboard(dashboards []metrics.Dashboard, name string) (metrics.Dashboard, bool) {
	for _, dash := range dashboards {
		if dash.Name == name {
			return dash, true
		}
	}
	return metrics.Dashboard{}, false
}

// applyDashboardOverride hides, reorders and re-ranges a dashboard by the tenant's override. Hidden names
// and order names match a tile's metric or a chart's id, so one name can address both; a name the
// dashboard does not have does nothing. Unnamed tiles and charts keep their pack order after the named ones.
func applyDashboardOverride(def contract.DashboardDefinition, o contract.DashboardOverride) contract.DashboardDefinition {
	hidden := map[string]bool{}
	for _, name := range o.Hidden {
		hidden[name] = true
	}
	def.Tiles = orderBy(dropWhere(def.Tiles, func(t contract.DashboardTile) bool { return hidden[t.Metric] }),
		func(t contract.DashboardTile) string { return t.Metric }, o.Order)
	def.Charts = orderBy(dropWhere(def.Charts, func(ch contract.DashboardChart) bool { return hidden[ch.ID] }),
		func(ch contract.DashboardChart) string { return ch.ID }, o.Order)
	if o.DefaultRange != "" {
		def.DefaultRange = o.DefaultRange
	}
	return def
}

// dropWhere returns the items drop does not match, in order.
func dropWhere[T any](items []T, drop func(T) bool) []T {
	out := make([]T, 0, len(items))
	for _, item := range items {
		if !drop(item) {
			out = append(out, item)
		}
	}
	return out
}

// orderBy returns items sorted by the position of their key in order. Keys order does not name keep their
// relative order after the named ones.
func orderBy[T any](items []T, key func(T) string, order []string) []T {
	rank := make(map[string]int, len(order))
	for i, name := range order {
		if _, seen := rank[name]; !seen {
			rank[name] = i
		}
	}
	rankOf := func(item T) int {
		if r, ok := rank[key(item)]; ok {
			return r
		}
		return len(order)
	}
	out := slices.Clone(items)
	slices.SortStableFunc(out, func(a, b T) int { return cmp.Compare(rankOf(a), rankOf(b)) })
	return out
}
