import { describe, it, expect } from 'vitest'
import { evaluateLisp, type EvalContext } from './lisp-eval'

// ---------------------------------------------------------------------------
// Basic arithmetic (matching Go TestLispSandbox_BasicArith)
// ---------------------------------------------------------------------------

describe('basic arithmetic', () => {
  it('(* quantity unit_price) = 50', () => {
    const ctx: EvalContext = {
      fields: { quantity: 10, unit_price: 5 },
      tables: {},
    }
    expect(evaluateLisp('(* quantity unit_price)', ctx)).toBe(50)
  })

  it('(+ a b c) = 35 (variadic addition)', () => {
    const ctx: EvalContext = {
      fields: { a: 10, b: 20, c: 5 },
      tables: {},
    }
    expect(evaluateLisp('(+ a b c)', ctx)).toBe(35)
  })
})

// ---------------------------------------------------------------------------
// SUM (matching Go TestLispSandbox_SUM)
// ---------------------------------------------------------------------------

describe('sum', () => {
  it('(sum "items" "amount") = 30', () => {
    const ctx: EvalContext = {
      fields: {},
      tables: {
        items: [{ amount: 10 }, { amount: 20 }],
      },
    }
    expect(evaluateLisp('(sum "items" "amount")', ctx)).toBe(30)
  })

  it('sum with empty table = 0', () => {
    const ctx: EvalContext = {
      fields: {},
      tables: { items: [] },
    }
    expect(evaluateLisp('(sum "items" "amount")', ctx)).toBe(0)
  })
})

// ---------------------------------------------------------------------------
// COUNT (matching Go TestLispSandbox_COUNT)
// ---------------------------------------------------------------------------

describe('count', () => {
  it('(count "items") = 2', () => {
    const ctx: EvalContext = {
      fields: {},
      tables: {
        items: [{ name: 'a' }, { name: 'b' }],
      },
    }
    expect(evaluateLisp('(count "items")', ctx)).toBe(2)
  })

  it('(count "items") with empty table = 0', () => {
    const ctx: EvalContext = {
      fields: {},
      tables: { items: [] },
    }
    expect(evaluateLisp('(count "items")', ctx)).toBe(0)
  })
})

// ---------------------------------------------------------------------------
// ROUND (matching Go TestLispSandbox_ROUND)
// ---------------------------------------------------------------------------

describe('round', () => {
  it('(round 10.567 2) = 10.57', () => {
    const ctx: EvalContext = { fields: {}, tables: {} }
    expect(evaluateLisp('(round 10.567 2)', ctx)).toBeCloseTo(10.57, 10)
  })
})

// ---------------------------------------------------------------------------
// DATEDIFF (matching Go TestLispSandbox_DATEDIFF)
// ---------------------------------------------------------------------------

describe('datediff', () => {
  it('(datediff "2026-06-28" "2026-07-01") = 3', () => {
    const ctx: EvalContext = { fields: {}, tables: {} }
    expect(evaluateLisp('(datediff "2026-06-28" "2026-07-01")', ctx)).toBe(3)
  })
})

// ---------------------------------------------------------------------------
// IF (matching Go TestLispSandbox_IF)
// ---------------------------------------------------------------------------

describe('if', () => {
  it('true branch: amount > 100 applies discount', () => {
    const ctx: EvalContext = { fields: { amount: 200 }, tables: {} }
    expect(evaluateLisp('(if (> amount 100) (* amount 0.9) amount)', ctx)).toBe(180)
  })

  it('false branch: amount <= 100 returns unchanged', () => {
    const ctx: EvalContext = { fields: { amount: 50 }, tables: {} }
    expect(evaluateLisp('(if (> amount 100) (* amount 0.9) amount)', ctx)).toBe(50)
  })
})

// ---------------------------------------------------------------------------
// CONCAT (matching Go TestLispSandbox_CONCAT)
// ---------------------------------------------------------------------------

describe('concat', () => {
  it('(concat "a" " " "b") = "a b"', () => {
    const ctx: EvalContext = { fields: {}, tables: {} }
    expect(evaluateLisp('(concat "a" " " "b")', ctx)).toBe('a b')
  })
})

// ---------------------------------------------------------------------------
// TODAY (matching Go TestLispSandbox_TODAY)
// ---------------------------------------------------------------------------

describe('today', () => {
  it('(today) returns current date as YYYY-MM-DD', () => {
    const ctx: EvalContext = { fields: {}, tables: {} }
    const result = evaluateLisp('(today)', ctx)
    const expected = new Date().toISOString().slice(0, 10)
    expect(result).toBe(expected)
  })
})

// ---------------------------------------------------------------------------
// Compound expressions (matching Go TestLispSandbox_CompoundWithSum)
// ---------------------------------------------------------------------------

describe('compound expressions', () => {
  it('(+ (sum "items" "line_total") surcharge) = 85', () => {
    const ctx: EvalContext = {
      fields: { surcharge: 5 },
      tables: {
        items: [{ line_total: 20 }, { line_total: 60 }],
      },
    }
    expect(evaluateLisp('(+ (sum "items" "line_total") surcharge)', ctx)).toBe(85)
  })

  it('(- (* qty price) discount) = 48 (deeply nested)', () => {
    const ctx: EvalContext = {
      fields: { qty: 10, price: 5, discount: 2 },
      tables: {},
    }
    expect(evaluateLisp('(- (* qty price) discount)', ctx)).toBe(48)
  })

  it('(* (sum "items" "amount") (- 1 (/ discount_pct 100))) = 270', () => {
    const ctx: EvalContext = {
      fields: { discount_pct: 10 },
      tables: {
        items: [{ amount: 100 }, { amount: 200 }],
      },
    }
    expect(evaluateLisp('(* (sum "items" "amount") (- 1 (/ discount_pct 100)))', ctx)).toBe(270)
  })
})

// ---------------------------------------------------------------------------
// String field values (matching Go TestLispSandbox_StringDocValue)
// ---------------------------------------------------------------------------

describe('string field values', () => {
  it('(concat greeting " " name) = "Hello Alice"', () => {
    const ctx: EvalContext = {
      fields: { greeting: 'Hello', name: 'Alice' },
      tables: {},
    }
    expect(evaluateLisp('(concat greeting " " name)', ctx)).toBe('Hello Alice')
  })
})

// ---------------------------------------------------------------------------
// Numeric doc fields + sum (matching Go TestLispSandbox_SumWithNumericDocFields)
// ---------------------------------------------------------------------------

describe('sum with numeric doc fields', () => {
  it('(- (sum "items" "amount") discount) = 95', () => {
    const ctx: EvalContext = {
      fields: { discount: 5 },
      tables: {
        items: [{ amount: 30 }, { amount: 70 }],
      },
    }
    expect(evaluateLisp('(- (sum "items" "amount") discount)', ctx)).toBe(95)
  })

  it('(- (sum "items" "amount") discount) with empty items = -5', () => {
    const ctx: EvalContext = {
      fields: { discount: 5 },
      tables: { items: [] },
    }
    expect(evaluateLisp('(- (sum "items" "amount") discount)', ctx)).toBe(-5)
  })
})

// ---------------------------------------------------------------------------
// Standalone atoms (matching Go TestLispSandbox_StringLiteral)
// ---------------------------------------------------------------------------

describe('standalone atoms', () => {
  it('"hello world" returns the string', () => {
    const ctx: EvalContext = { fields: {}, tables: {} }
    expect(evaluateLisp('"hello world"', ctx)).toBe('hello world')
  })

  it('42 returns the number', () => {
    const ctx: EvalContext = { fields: {}, tables: {} }
    expect(evaluateLisp('42', ctx)).toBe(42)
  })

  it('a field reference returns the field value', () => {
    const ctx: EvalContext = { fields: { x: 10 }, tables: {} }
    expect(evaluateLisp('x', ctx)).toBe(10)
  })
})

// ---------------------------------------------------------------------------
// Error handling (matching Go TestLispSandbox_MissingSymbol)
// ---------------------------------------------------------------------------

describe('error handling', () => {
  it('undefined symbol returns null', () => {
    const ctx: EvalContext = { fields: { a: 10 }, tables: {} }
    expect(evaluateLisp('(+ a b)', ctx)).toBeNull()
  })

  it('empty expression returns null', () => {
    const ctx: EvalContext = { fields: {}, tables: {} }
    expect(evaluateLisp('', ctx)).toBeNull()
    expect(evaluateLisp('  ', ctx)).toBeNull()
  })
})

// ---------------------------------------------------------------------------
// Integer field values (matching Go TestLispSandbox_WithIntValues)
// ---------------------------------------------------------------------------

describe('integer field values', () => {
  it('(* quantity unit_price) with ints = 30', () => {
    const ctx: EvalContext = {
      fields: { quantity: 3, unit_price: 10 },
      tables: {},
    }
    expect(evaluateLisp('(* quantity unit_price)', ctx)).toBe(30)
  })
})

// ---------------------------------------------------------------------------
// Isolation across calls (matching Go TestLispSandbox_MultipleEvalCalls)
// ---------------------------------------------------------------------------

describe('isolation across calls', () => {
  it('multiple eval calls do not pollute each other', () => {
    const ctx1: EvalContext = { fields: { x: 10 }, tables: {} }
    expect(evaluateLisp('(* x 2)', ctx1)).toBe(20)

    const ctx2: EvalContext = { fields: { y: 5 }, tables: {} }
    expect(evaluateLisp('(* y 3)', ctx2)).toBe(15)
  })
})
