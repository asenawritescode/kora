package analytics

import (
	"crypto/sha256"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"
)

// cacheEntry holds a cached query result with expiry.
type cacheEntry struct {
	result   *QueryResult
	expiresAt time.Time
}

// QueryEngine resolves metric queries against the rollup tables.
type QueryEngine struct {
	DB       *sql.DB
	SiteName string
	cache    sync.Map // key string → *cacheEntry
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
	h := sha256.New()
	h.Write([]byte(qe.SiteName))
	h.Write([]byte(metric))
	h.Write([]byte(req.From))
	h.Write([]byte(req.To))
	h.Write([]byte(req.GroupBy))
	return fmt.Sprintf("%x", h.Sum(nil))
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
	Metric    string   `json:"metric"`
	From      string   `json:"from,omitempty"`   // ISO date, inclusive
	To        string   `json:"to,omitempty"`     // ISO date, inclusive
	GroupBy   string   `json:"group_by,omitempty"` // "day" | "week" | "month"
	Limit     int      `json:"limit,omitempty"`
}

// QueryResult holds the result of a metric query.
type QueryResult struct {
	Metric  string           `json:"metric"`
	Columns []string         `json:"columns"`
	Rows    []map[string]any `json:"rows"`
	Total   int              `json:"total"`
}

// Resolve executes a pre-computed metric query against the rollup tables.
// Metrics are stored as rows in _kora_analytics_daily (or _monthly for month+ ranges).
// The query is a simple read of pre-aggregated values — no scanning of business tables.
func (qe *QueryEngine) Resolve(metric *Metric, req QueryRequest) (*QueryResult, error) {
	from, to := parseDateRange(req.From, req.To)
	table := "_kora_analytics_daily"
	dateCol := "date"

	// Use monthly table for ranges >= 60 days.
	if to.Sub(from) >= 60*24*time.Hour {
		table = "_kora_analytics_monthly"
		dateCol = "month"
	}

	var rows *sql.Rows
	var err error

	switch metric.Type {
	case MetricCount, MetricCountByField, MetricCountByLinkedField, MetricCountByTime,
		MetricSum, MetricStateDistribution:
		rows, err = qe.queryAggregate(table, dateCol, metric, from, to, req.GroupBy)

	case MetricAvg:
		// Average requires two queries: sum / count.
		return qe.queryAvg(table, dateCol, metric, from, to, req.GroupBy)

	default:
		rows, err = qe.queryAggregate(table, dateCol, metric, from, to, req.GroupBy)
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

	for rows.Next() {
		row := make(map[string]any)
		scanTargets := make([]any, len(cols))
		for i := range cols {
			var v any
			scanTargets[i] = &v
		}
		if err := rows.Scan(scanTargets...); err != nil {
			return nil, fmt.Errorf("scan row: %w", err)
		}
		for i, col := range cols {
			row[col] = *(scanTargets[i].(*any))
		}
		result.Rows = append(result.Rows, row)
	}
	result.Total = len(result.Rows)

	return result, nil
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
		if err != nil || result.Total == 0 {
			// Try monthly for trend metrics.
			req = QueryRequest{Metric: m.Name, From: monthAgo, To: today}
			result, err = qe.Resolve(m, req)
		}
		if err == nil && result.Total > 0 {
			switch m.Type {
			case MetricCount, MetricSum:
				if len(result.Rows) > 0 {
					// First row, first numeric column (usually "value").
					for _, v := range result.Rows[0] {
						insights[m.Name] = v
						break
					}
				}
			case MetricCountByField, MetricStateDistribution:
				byField := make(map[string]any)
				for _, row := range result.Rows {
					dim, _ := row["dimension"].(string)
					val, _ := row["value"].(float64)
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
func (qe *QueryEngine) queryAggregate(table, dateCol string, metric *Metric, from, to time.Time, groupBy string) (*sql.Rows, error) {
	fromStr := from.Format("2006-01-02")
	toStr := to.Format("2006-01-02")

	selectClause := "dimension, SUM(value) AS value"
	groupClause := "dimension"
	orderClause := "value DESC"

	if groupBy == "day" || groupBy == "week" || groupBy == "month" {
		selectClause = fmt.Sprintf("%s AS bucket, SUM(value) AS value", dateCol)
		groupClause = dateCol
		orderClause = "bucket ASC"
	}

	query := fmt.Sprintf(
		`SELECT %s FROM %s
		 WHERE site = ? AND doctype = ? AND metric = ?
		   AND %s >= ? AND %s <= ?
		 GROUP BY %s
		 ORDER BY %s`,
		selectClause, table, dateCol, dateCol, groupClause, orderClause,
	)

	return qe.DB.Query(query, qe.SiteName, metric.DocType, metric.Name, fromStr, toStr)
}

// queryAvg computes average by dividing sum by count for a time range.
func (qe *QueryEngine) queryAvg(table, dateCol string, metric *Metric, from, to time.Time, groupBy string) (*QueryResult, error) {
	fromStr := from.Format("2006-01-02")
	toStr := to.Format("2006-01-02")

	// For avg metrics, we need sum(value) / count of non-zero days.
	// Since rollup values are accumulated deltas, the sum is the total over the period.
	query := fmt.Sprintf(
		`SELECT SUM(value) AS total, COUNT(*) AS days
		 FROM %s
		 WHERE site = ? AND doctype = ? AND metric = ?
		   AND %s >= ? AND %s <= ?`,
		table, dateCol, dateCol,
	)

	var total, days float64
	if err := qe.DB.QueryRow(query, qe.SiteName, metric.DocType, metric.Name, fromStr, toStr).Scan(&total, &days); err != nil {
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
	Enabled         bool   `json:"enabled"`
	EventsProcessed int64  `json:"events_processed"`
	EventsDropped   int64  `json:"events_dropped"`
}

// GetStatus returns the current analytics pipeline status.
func GetStatus(bus EventBus) *Status {
	s := &Status{Enabled: bus != nil}
	if bus != nil {
		s.EventsDropped = bus.Dropped()
	}
	return s
}
