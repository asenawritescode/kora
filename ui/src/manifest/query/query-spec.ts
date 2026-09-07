export type QueryOperator = 'equals' | 'contains' | 'gt' | 'gte' | 'lt' | 'lte' | 'between'
export type QueryVisualization = 'metric' | 'table' | 'bar' | 'line'

export interface QueryFilter {
  field: string
  operator: QueryOperator
  value: string | number | [string, string]
}

export interface QueryMetric {
  type: 'count' | 'sum' | 'average'
  field?: string
  label: string
}

export interface QuerySpec {
  resource: string
  filters: QueryFilter[]
  groupBy: string[]
  metrics: QueryMetric[]
  columns: string[]
  visualization: QueryVisualization
}

export function validateQuerySpec(query: QuerySpec, fields: readonly string[]): string[] {
  const allowed = new Set(fields)
  const issues: string[] = []
  for (const filter of query.filters) if (!allowed.has(filter.field)) issues.push(`Unknown filter field: ${filter.field}`)
  for (const field of query.groupBy) if (!allowed.has(field)) issues.push(`Unknown grouping field: ${field}`)
  for (const field of query.columns) if (!allowed.has(field)) issues.push(`Unknown column: ${field}`)
  for (const metric of query.metrics) if (metric.field && !allowed.has(metric.field)) issues.push(`Unknown metric field: ${metric.field}`)
  return issues
}
