package analytics

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/db"
)

// cacheEntry holds a cached query result with expiry.
type cacheEntry struct {
	result    *QueryResult
	expiresAt time.Time
}

// QueryEngine resolves metric queries against the rollup tables.
type QueryEngine struct {
	DB            *sql.DB
	SiteName      string
	Dialect       kdb.Dialect
	BusinessScope contract.BusinessScope
	cache         sync.Map // key string → *cacheEntry
}

// CachedResolve returns a cached result if available and not expired.
// Otherwise, delegates to Resolve and caches the result for the given TTL.
func (qe *QueryEngine) CachedResolve(metric *Metric, req QueryRequest, ttl time.Duration) (*QueryResult, error) {
	key := qe.cacheKey(metric.Name, req)
	if entry, ok := qe.cache.Load(key); ok {
		if e := entry.(*cacheEntry); time.Now().Before(e.expiresAt) {
			return e.result, nil
		}
		qe.cache.Delete(key)
	}

	result, err := qe.Resolve(metric, req)
	if err != nil {
		return nil, err
	}

	qe.cache.Store(key, &cacheEntry{
		result:    result,
		expiresAt: time.Now().Add(ttl),
	})
	return result, nil
}

func (qe *QueryEngine) cacheKey(metric string, req QueryRequest) string {
	// Include every query input. Omitting filters or limits can return a cached
	// result for a different authorized query in the same tenant.
	encoded, err := json.Marshal(req)
	if err != nil {
		return qe.SiteName + "\x00" + metric + "\x00invalid"
	}
	return qe.SiteName + "\x00" + metric + "\x00" + string(encoded)
}

// InvalidateCache clears all cached results for this engine.
func (qe *QueryEngine) InvalidateCache() {
	qe.cache.Range(func(key, _ any) bool {
		qe.cache.Delete(key)
		return true
	})
}

// QueryRequest holds parameters for a metric query.
type QueryRequest struct {
	Metric    string           `json:"metric"`
	From      string           `json:"from,omitempty"`     // ISO date, inclusive
	To        string           `json:"to,omitempty"`       // ISO date, inclusive
	GroupBy   string           `json:"group_by,omitempty"` // "day" | "week" | "month"
	Dimension string           `json:"dimension,omitempty"`
	Filters   []SemanticFilter `json:"filters,omitempty"`
	Limit     int              `json:"limit,omitempty"`
}

// QueryResult holds the result of a metric query.
type QueryResult struct {
	Metric  string           `json:"metric"`
	Columns []string         `json:"columns"`
	Rows    []map[string]any `json:"rows"`
	Total   int              `json:"total"`
}

func (req QueryRequest) Validate() error {
	if req.Limit < 0 || req.Limit > MaxSemanticResultRows {
		return fmt.Errorf("analytics result limit must be between 0 and %d", MaxSemanticResultRows)
	}
	if req.GroupBy != "" && !validGranularity(req.GroupBy) {
		return fmt.Errorf("unsupported analytics grouping %q", req.GroupBy)
	}
	from, to := parseDateRange(req.From, req.To)
	if (req.From != "" && from.Format("2006-01-02") != req.From) || (req.To != "" && to.Format("2006-01-02") != req.To) {
		return fmt.Errorf("analytics dates must be valid ISO dates")
	}
	if from.After(to) {
		return fmt.Errorf("analytics start date must not be after end date")
	}
	if int(to.Sub(from).Hours()/24)+1 > MaxSemanticDateRangeDays {
		return fmt.Errorf("analytics date range exceeds %d days", MaxSemanticDateRangeDays)
	}
	if len(req.Filters) > MaxSemanticFilters || (len(req.Filters) > 0 && req.Dimension == "") {
		return fmt.Errorf("analytics filters require one declared dimension and no more than %d filters", MaxSemanticFilters)
	}
	for _, filter := range req.Filters {
		if filter.Field != req.Dimension || len(filter.Values) == 0 || len(filter.Values) > MaxSemanticFilterValues || !validSemanticFilterOperator(filter.Operator) {
			return fmt.Errorf("analytics filter is invalid")
		}
		op := strings.ToLower(strings.TrimSpace(filter.Operator))
		if (op == "=" || op == "==" || op == "!=" || op == "<>" || op == "like" || op == "not like") && len(filter.Values) != 1 {
			return fmt.Errorf("analytics filter operator %q requires exactly one value", filter.Operator)
		}
		for _, value := range filter.Values {
			if len(value) > MaxSemanticFilterBytes {
				return fmt.Errorf("analytics filter value exceeds %d bytes", MaxSemanticFilterBytes)
			}
		}
	}
	return nil
}

// Resolve executes a pre-computed metric query against the rollup tables.
// Metrics are stored as rows in _kora_analytics_daily (or _monthly for month+ ranges).
// The query is a simple read of pre-aggregated values — no scanning of business tables.
func (qe *QueryEngine) Resolve(metric *Metric, req QueryRequest) (*QueryResult, error) {
	if metric == nil {
		return nil, fmt.Errorf("analytics metric is required")
	}
	if err := req.Validate(); err != nil {
		return nil, err
	}
	limit := req.Limit
	if limit == 0 {
		limit = MaxSemanticResultRows
	}
	from, to := parseDateRange(req.From, req.To)
	table := "_kora_analytics_daily"
	dateCol := "date"

	// Monthly rollups preserve an exact requested range only when it starts on
	// the first day and ends on the last day of a month. All other ranges use
	// daily rollups so inclusive boundary dates remain exact.
	hasTimeFilter := false
	for _, filter := range req.Filters {
		if filter.Field == metric.TimeField {
			hasTimeFilter = true
			break
		}
	}
	if req.GroupBy != "day" && req.GroupBy != "week" && !hasTimeFilter && monthlyRangeAligned(from, to) && to.Sub(from) >= 60*24*time.Hour {
		table = "_kora_analytics_monthly"
		dateCol = "month"
	}

	var rows *sql.Rows
	var err error

	switch metric.Type {
	case MetricCount, MetricCountByField, MetricCountByLinkedField, MetricCountByTime,
		MetricSum, MetricStateDistribution:
		rows, err = qe.queryAggregate(table, dateCol, metric, from, to, req, limit)

	case MetricAvg:
		if req.GroupBy != "" || req.Dimension != "" || len(req.Filters) != 0 {
			return nil, fmt.Errorf("average metrics support only an overall time-range query")
		}
		// Average requires two queries: sum / count.
		return qe.queryAvg(table, dateCol, metric, from, to, req.GroupBy)

	default:
		rows, err = qe.queryAggregate(table, dateCol, metric, from, to, req, limit)
	}

	if err != nil {
		return nil, fmt.Errorf("analytics query: %w", err)
	}
	defer rows.Close()

	result := &QueryResult{
		Metric: metric.Name,
	}

	cols, _ := rows.Columns()
	result.Columns = cols

	// Pre-allocate scan targets once, reuse per row to reduce GC pressure.
	scanTargets := make([]any, len(cols))
	scanVals := make([]any, len(cols))
	for i := range cols {
		scanTargets[i] = &scanVals[i]
	}

	for rows.Next() {
		if err := rows.Scan(scanTargets...); err != nil {
			return nil, fmt.Errorf("scan row: %w", err)
		}
		row := make(map[string]any, len(cols))
		for i, col := range cols {
			if b, ok := scanVals[i].([]byte); ok {
				row[col] = string(b)
			} else {
				row[col] = scanVals[i]
			}
		}
		result.Rows = append(result.Rows, row)
	}
	result.Total = len(result.Rows)

	return result, nil
}

func monthlyRangeAligned(from, to time.Time) bool {
	return from.Day() == 1 && to.AddDate(0, 0, 1).Day() == 1
}

// ResolveInsights returns pre-computed numbers for a doctype — designed for AI consumption.
// Returns a flat map of metric_name → current value, plus trend indicators.
func (qe *QueryEngine) ResolveInsights(doctype string, metrics []*Metric) (map[string]any, error) {
	insights := make(map[string]any)
	today := time.Now().Format("2006-01-02")
	monthAgo := time.Now().AddDate(0, -1, 0).Format("2006-01-02")

	for _, m := range metrics {
		if m.DocType != doctype {
			continue
		}
		req := QueryRequest{Metric: m.Name, From: today, To: today}
		result, err := qe.Resolve(m, req)
		// Older or event-only rollups may have the creation trend without the
		// companion count metric. Use that governed metric as a count fallback.
		if (err != nil || result.Total == 0) && m.Type == MetricCount {
			fallback := *m
			fallback.Name = metricName(doctype) + "_created_daily"
			fallback.Type = MetricCountByTime
			result, err = qe.Resolve(&fallback, req)
		}
		if err != nil || result.Total == 0 {
			// Try monthly for trend metrics.
			req = QueryRequest{Metric: m.Name, From: monthAgo, To: today}
			result, err = qe.Resolve(m, req)
			if (err != nil || result.Total == 0) && m.Type == MetricCount {
				fallback := *m
				fallback.Name = metricName(doctype) + "_created_daily"
				fallback.Type = MetricCountByTime
				result, err = qe.Resolve(&fallback, req)
			}
		}
		if err == nil && result.Total > 0 {
			switch m.Type {
			case MetricCount, MetricSum:
				if len(result.Rows) > 0 {
					if v, ok := result.Rows[0]["value"]; ok {
						insights[m.Name] = v
					}
				}
			case MetricCountByField, MetricStateDistribution:
				byField := make(map[string]any)
				for _, row := range result.Rows {
					dim := dbString(row["dimension"])
					val := dbFloat(row["value"])
					if dim != "" {
						// Strip "fieldname=" prefix for cleaner AI consumption.
						if idx := strings.Index(dim, "="); idx >= 0 {
							dim = dim[idx+1:]
						}
						byField[dim] = val
					}
				}
				insights[m.Name] = byField
			}
		}
	}

	return insights, nil
}

// queryAggregate runs a simple aggregate query against a rollup table.
func (qe *QueryEngine) queryAggregate(table, dateCol string, metric *Metric, from, to time.Time, req QueryRequest, limit int) (*sql.Rows, error) {
	fromStr := from.Format("2006-01-02")
	toStr := to.Format("2006-01-02")

	selectClause := "dimension, SUM(value) AS value"
	groupClause := "dimension"
	orderClause := "value DESC"

	if req.GroupBy != "" {
		bucketExpr, err := analyticsPeriodExpression(qe.Dialect, dateCol, req.GroupBy)
		if err != nil {
			return nil, err
		}
		selectClause = fmt.Sprintf("%s AS bucket, SUM(value) AS value", bucketExpr)
		groupClause = bucketExpr
		orderClause = "bucket ASC"
	}

	scope := qe.BusinessScope
	if scope.TenantID == "" {
		scope = contract.BusinessScope{TenantID: contract.TenantID(qe.SiteName), Kind: contract.ScopeKindTenant}
	}
	where := []string{"site = ?", "scope_kind = ?", "legal_entity_id = ?", "doctype = ?", "metric = ?", dateCol + " >= ?", dateCol + " <= ?"}
	args := []any{qe.SiteName, string(scope.Kind), scope.LegalEntityID, metric.DocType, metric.Name, fromStr, toStr}
	if len(req.Filters) > 0 {
		column := "dimension"
		prefix := req.Dimension + "="
		if req.Dimension == metric.TimeField {
			column = dateCol
			prefix = ""
		}
		for _, filter := range req.Filters {
			clause, values := semanticFilterSQL(qe.Dialect, column, prefix, filter)
			where = append(where, clause)
			args = append(args, values...)
		}
	}
	query := fmt.Sprintf(
		`SELECT %s FROM %s WHERE %s GROUP BY %s ORDER BY %s LIMIT ?`,
		selectClause, table, strings.Join(where, " AND "), groupClause, orderClause,
	)
	args = append(args, limit)
	return qe.DB.Query(kdb.Rebind(qe.Dialect, query), args...)
}

func analyticsPeriodExpression(dialect kdb.Dialect, dateCol, granularity string) (string, error) {
	column := dialect.QuoteIdent(dateCol)
	switch dialect.DriverName() {
	case "mysql":
		switch granularity {
		case "day":
			return "DATE_FORMAT(" + column + ", '%Y-%m-%d')", nil
		case "week":
			return "DATE_FORMAT(DATE_SUB(" + column + ", INTERVAL WEEKDAY(" + column + ") DAY), '%Y-%m-%d')", nil
		case "month":
			return "DATE_FORMAT(" + column + ", '%Y-%m-01')", nil
		case "quarter":
			return "CONCAT(YEAR(" + column + "), '-Q', QUARTER(" + column + "))", nil
		case "year":
			return "CAST(YEAR(" + column + ") AS CHAR)", nil
		}
	case "postgres":
		switch granularity {
		case "day":
			return "TO_CHAR(" + column + ", 'YYYY-MM-DD')", nil
		case "week":
			return "TO_CHAR((" + column + "::date - (EXTRACT(ISODOW FROM " + column + ")::int - 1)), 'YYYY-MM-DD')", nil
		case "month":
			return "TO_CHAR(" + column + ", 'YYYY-MM') || '-01'", nil
		case "quarter":
			return "TO_CHAR(" + column + ", 'YYYY') || '-Q' || EXTRACT(QUARTER FROM " + column + ")::int::text", nil
		case "year":
			return "TO_CHAR(" + column + ", 'YYYY')", nil
		}
	case "libsql":
		switch granularity {
		case "day":
			return "STRFTIME('%Y-%m-%d', " + column + ")", nil
		case "week":
			return "DATE(" + column + ", '-' || ((CAST(STRFTIME('%w', " + column + ") AS INTEGER) + 6) % 7) || ' days')", nil
		case "month":
			return "STRFTIME('%Y-%m', " + column + ") || '-01'", nil
		case "quarter":
			return "STRFTIME('%Y', " + column + ") || '-Q' || CAST(((CAST(STRFTIME('%m', " + column + ") AS INTEGER) - 1) / 3 + 1) AS TEXT)", nil
		case "year":
			return "STRFTIME('%Y', " + column + ")", nil
		}
	}
	return "", fmt.Errorf("unsupported analytics time grouping %q for %s", granularity, dialect.DriverName())
}

func semanticFilterSQL(dialect kdb.Dialect, column, prefix string, filter SemanticFilter) (string, []any) {
	q := dialect.QuoteIdent
	column = q(column)
	op := strings.ToLower(strings.TrimSpace(filter.Operator))
	values := make([]any, len(filter.Values))
	for i, value := range filter.Values {
		values[i] = prefix + value
	}
	switch op {
	case "=", "==", "!=", "<>":
		if op == "==" {
			op = "="
		}
		return column + " " + op + " ?", values[:1]
	case "in", "not in":
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(values)), ",")
		return column + " " + strings.ToUpper(op) + " (" + placeholders + ")", values
	case "like", "not like":
		escape := strings.NewReplacer("!", "!!", "%", "!%", "_", "!_")
		pattern := escape.Replace(prefix) + "%" + escape.Replace(filter.Values[0]) + "%"
		return "LOWER(" + column + ") " + strings.ToUpper(op) + " LOWER(?) ESCAPE '!'", []any{pattern}
	default:
		return "1 = 0", nil
	}
}

// queryAvg computes average by dividing sum by count for a time range.
func (qe *QueryEngine) queryAvg(table, dateCol string, metric *Metric, from, to time.Time, groupBy string) (*QueryResult, error) {
	fromStr := from.Format("2006-01-02")
	toStr := to.Format("2006-01-02")

	// For avg metrics, we need sum(value) / count of non-zero days.
	// Since rollup values are accumulated deltas, the sum is the total over the period.
	scope := qe.BusinessScope
	if scope.TenantID == "" {
		scope = contract.BusinessScope{TenantID: contract.TenantID(qe.SiteName), Kind: contract.ScopeKindTenant}
	}
	query := fmt.Sprintf(
		`SELECT SUM(value) AS total, COUNT(*) AS days
		 FROM %s
		 WHERE site = ? AND scope_kind = ? AND legal_entity_id = ? AND doctype = ? AND metric = ?
		   AND %s >= ? AND %s <= ?`,
		table, dateCol, dateCol,
	)

	var total, days float64
	if err := qe.DB.QueryRow(kdb.Rebind(qe.Dialect, query), qe.SiteName, string(scope.Kind), scope.LegalEntityID, metric.DocType, metric.Name, fromStr, toStr).Scan(&total, &days); err != nil {
		return nil, err
	}

	avg := 0.0
	if days > 0 {
		avg = total / days
	}

	return &QueryResult{
		Metric:  metric.Name,
		Columns: []string{"value"},
		Rows:    []map[string]any{{"value": avg}},
		Total:   1,
	}, nil
}

// parseDateRange parses optional ISO date strings and returns sensible defaults.
func parseDateRange(fromStr, toStr string) (time.Time, time.Time) {
	now := time.Now()
	to := now
	from := now.AddDate(0, 0, -30) // default: last 30 days

	if fromStr != "" {
		if t, err := time.Parse("2006-01-02", fromStr); err == nil {
			from = t
		}
	}
	if toStr != "" {
		if t, err := time.Parse("2006-01-02", toStr); err == nil {
			to = t
		}
	}
	return from, to
}

// Status holds the analytics pipeline status for a site.
type Status struct {
	Enabled         bool  `json:"enabled"`
	EventsProcessed int64 `json:"events_processed"`
	EventsDropped   int64 `json:"events_dropped"`
}

// GetStatus returns the current analytics pipeline status.
func GetStatus(bus EventBus) *Status {
	s := &Status{Enabled: bus != nil}
	if bus != nil {
		s.EventsDropped = bus.Dropped()
	}
	return s
}

// dbString safely extracts a string from a DB-scanned value.
// MySQL driver returns []byte for VARCHAR; LibSQL returns string.
func dbString(v any) string {
	switch val := v.(type) {
	case string:
		return val
	case []byte:
		return string(val)
	default:
		return ""
	}
}

// dbFloat safely extracts a float64 from a DB-scanned value.
func dbFloat(v any) float64 {
	switch val := v.(type) {
	case float64:
		return val
	case int64:
		return float64(val)
	case []byte:
		var f float64
		fmt.Sscanf(string(val), "%f", &f)
		return f
	default:
		return 0
	}
}
