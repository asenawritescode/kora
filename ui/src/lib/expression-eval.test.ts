import { describe, it, expect } from 'vitest'
import { evaluateComputed, findDependencies } from './expression-eval'

interface GoldenVector {
  expr: string
  inputs: Record<string, any>
  expected: number | null
}

const goldenVectors: GoldenVector[] = [
  { expr: 'SUM(items.amount)', inputs: { items: [{ amount: 10 }, { amount: 20 }] }, expected: 30 },
  { expr: 'ROUND(price * qty, 2)', inputs: { price: 10.5, qty: 3 }, expected: 31.5 },
  { expr: 'COUNT(attendees)', inputs: { attendees: [{ name: 'a' }, { name: 'b' }] }, expected: 2 },
  { expr: 'DATEDIFF(start_date, end_date)', inputs: { start_date: '2026-06-29', end_date: '2026-07-01' }, expected: 2 },
  { expr: 'DATEDIFF(today(), due_date)', inputs: { due_date: '2026-07-01' }, expected: null },
  { expr: 'qty * unit_price', inputs: { qty: 5, unit_price: 12.5 }, expected: 62.5 },
  { expr: 'SUM(items.line_total)', inputs: { items: [{ line_total: 20 }, { line_total: 60 }] }, expected: 80 },
  { expr: 'ROUND(total / 3, 0)', inputs: { total: 100 }, expected: 33 },
  { expr: 'qty * price - discount', inputs: { qty: 10, price: 5, discount: 2 }, expected: 48 },
]

describe('Golden computed expression parity', () => {
  for (const v of goldenVectors) {
    it(`evaluates ${v.expr} = ${v.expected ?? 'dynamic'}`, () => {
      const ctx = toEvalContext(v.inputs)

      // Handle today() dynamically.
      let expected = v.expected
      if (v.expr.includes('today()') && expected === null) {
        expected = computeTodayExpected(v.inputs)
        const result = evaluateComputed(v.expr, ctx)
        expect(result).toBeCloseTo(expected!, 0)
        return
      }

      const result = evaluateComputed(v.expr, ctx)
      expect(result).toBeCloseTo(expected!, 10)
    })
  }
})

describe('findDependencies', () => {
  it('skips SUM keyword', () => {
    const deps = findDependencies('SUM(items.amount)')
    expect(deps).not.toContain('sum')
  })

  it('skips ROUND keyword', () => {
    const deps = findDependencies('ROUND(price * qty, 2)')
    expect(deps).not.toContain('round')
  })

  it('skips COUNT keyword', () => {
    const deps = findDependencies('COUNT(attendees)')
    expect(deps).not.toContain('count')
  })

  it('skips DATEDIFF keyword', () => {
    const deps = findDependencies('DATEDIFF(a, b)')
    expect(deps).not.toContain('datediff')
  })

  it('skips today keyword', () => {
    const deps = findDependencies('DATEDIFF(today(), due_date)')
    expect(deps).not.toContain('today')
  })

  it('finds field references', () => {
    const deps = findDependencies('qty * price - discount')
    expect(deps).toContain('qty')
    expect(deps).toContain('price')
    expect(deps).toContain('discount')
  })
})

function toEvalContext(inputs: Record<string, any>): { fields: Record<string, any>; tables: Record<string, Record<string, any>[]> } {
  const fields: Record<string, any> = {}
  const tables: Record<string, Record<string, any>[]> = {}

  for (const [key, val] of Object.entries(inputs)) {
    if (Array.isArray(val)) {
      tables[key] = val as Record<string, any>[]
    } else {
      fields[key] = val
    }
  }

  return { fields, tables }
}

function computeTodayExpected(inputs: Record<string, any>): number {
  const today = new Date()
  today.setHours(0, 0, 0, 0)

  // DATEDIFF(today(), due_date) — due_date is an input field
  if (inputs.due_date) {
    const dueDate = new Date(inputs.due_date as string)
    dueDate.setHours(0, 0, 0, 0)
    // DATEDIFF(a, b) = int(b - a) in days
    const diffMs = dueDate.getTime() - today.getTime()
    return Math.round(diffMs / (1000 * 60 * 60 * 24))
  }

  return 0
}
