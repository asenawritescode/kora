import type { QuerySpec } from './query-spec'

export interface QueryAnswer {
  rows: Array<Record<string, unknown>>
  metrics: Array<{ label: string; value: number }>
  total: number
}

export function executeLocalQuery(query: QuerySpec, source: unknown): QueryAnswer {
  const rows = Array.isArray((source as { data?: unknown })?.data) ? (source as { data: Array<Record<string, unknown>> }).data : []
  const filtered = rows.filter((row) => query.filters.every((filter) => compare(row[filter.field], filter.operator, filter.value)))
  const metrics = query.metrics.map((metric) => {
    const values = metric.field ? filtered.map((row) => Number(row[metric.field!] ?? 0)) : []
    const value = metric.type === 'count' ? filtered.length : metric.type === 'average'
      ? (values.length ? values.reduce((sum, item) => sum + item, 0) / values.length : 0)
      : values.reduce((sum, item) => sum + item, 0)
    return { label: metric.label, value }
  })
  return { rows: filtered, metrics, total: filtered.length }
}

function compare(value: unknown, operator: QuerySpec['filters'][number]['operator'], expected: QuerySpec['filters'][number]['value']) {
  if (operator === 'contains') return String(value ?? '').toLowerCase().includes(String(expected).toLowerCase())
  if (operator === 'between' && Array.isArray(expected)) return String(value) >= expected[0] && String(value) <= expected[1]
  if (operator === 'equals') return String(value ?? '') === String(expected)
  const left = Number(value); const right = Number(expected)
  if (operator === 'gt') return left > right
  if (operator === 'gte') return left >= right
  if (operator === 'lt') return left < right
  return left <= right
}
