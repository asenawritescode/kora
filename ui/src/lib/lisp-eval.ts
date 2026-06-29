/**
 * Safe s-expression evaluator for Kora computed fields.
 *
 * Recursive-descent parser + whitelist evaluator for a safe subset of s-expressions.
 * No Function constructor, no eval, no code injection.
 *
 * Matches the Go LispSandbox (doctype/lisp.go) semantics.
 */

export interface EvalContext {
  /** Current field values (scalar fields on this document). */
  fields: Record<string, any>
  /** Child table rows, keyed by table fieldname. */
  tables: Record<string, Record<string, any>[]>
}

// ---------------------------------------------------------------------------
// Tokenizer
// ---------------------------------------------------------------------------

type TokenType = 'lparen' | 'rparen' | 'number' | 'string' | 'symbol'

interface Token {
  type: TokenType
  value: number | string
}

function tokenize(input: string): Token[] {
  const tokens: Token[] = []
  let i = 0

  while (i < input.length) {
    const ch = input[i]

    // Whitespace
    if (/\s/.test(ch)) {
      i++
      continue
    }

    if (ch === '(') {
      tokens.push({ type: 'lparen', value: '' })
      i++
      continue
    }
    if (ch === ')') {
      tokens.push({ type: 'rparen', value: '' })
      i++
      continue
    }

    // String literal "..." — no escape sequences except backslash
    if (ch === '"') {
      let s = ''
      i++ // skip opening quote
      while (i < input.length && input[i] !== '"') {
        if (input[i] === '\\' && i + 1 < input.length) {
          i++
          s += input[i]
        } else {
          s += input[i]
        }
        i++
      }
      i++ // skip closing quote
      tokens.push({ type: 'string', value: s })
      continue
    }

    // Number literal (including negative numbers like -5)
    if (/[0-9]/.test(ch) || (ch === '-' && i + 1 < input.length && /[0-9]/.test(input[i + 1]))) {
      let num = ''
      let dotSeen = false
      if (ch === '-') {
        num += '-'
        i++
      }
      while (i < input.length && /[0-9.]/.test(input[i])) {
        if (input[i] === '.') {
          if (dotSeen) break // second dot ends the number
          dotSeen = true
        }
        num += input[i]
        i++
      }
      tokens.push({ type: 'number', value: parseFloat(num) })
      continue
    }

    // Symbol — everything else that is not whitespace or parenthesis
    let sym = ''
    while (i < input.length && !/\s/.test(input[i]) && input[i] !== '(' && input[i] !== ')') {
      sym += input[i]
      i++
    }
    if (sym.length > 0) {
      tokens.push({ type: 'symbol', value: sym })
    }
  }

  return tokens
}

// ---------------------------------------------------------------------------
// AST
// ---------------------------------------------------------------------------

interface SexpAtom {
  type: 'atom'
  valueType: 'number' | 'string' | 'symbol'
  value: number | string
}

interface SexpList {
  type: 'list'
  children: SexpNode[]
}

type SexpNode = SexpAtom | SexpList

// ---------------------------------------------------------------------------
// Parser (recursive descent)
// ---------------------------------------------------------------------------

class ParseError extends Error {
  constructor(message: string) {
    super(message)
    this.name = 'ParseError'
  }
}

function parseTokens(tokens: Token[], pos: number): { node: SexpNode; next: number } {
  if (pos >= tokens.length) {
    throw new ParseError('Unexpected end of expression')
  }

  const token = tokens[pos]

  switch (token.type) {
    case 'lparen': {
      const children: SexpNode[] = []
      let i = pos + 1
      while (i < tokens.length && tokens[i].type !== 'rparen') {
        const result = parseTokens(tokens, i)
        children.push(result.node)
        i = result.next
      }
      if (i >= tokens.length) {
        throw new ParseError('Unclosed parenthesis')
      }
      return { node: { type: 'list', children }, next: i + 1 }
    }
    case 'rparen':
      throw new ParseError('Unexpected closing parenthesis')
    case 'number':
      return {
        node: { type: 'atom', valueType: 'number', value: token.value as number },
        next: pos + 1,
      }
    case 'string':
      return {
        node: { type: 'atom', valueType: 'string', value: token.value as string },
        next: pos + 1,
      }
    case 'symbol':
      return {
        node: { type: 'atom', valueType: 'symbol', value: token.value as string },
        next: pos + 1,
      }
  }
}

function parseExpression(input: string): SexpNode {
  const tokens = tokenize(input)
  if (tokens.length === 0) {
    throw new ParseError('Empty expression')
  }
  const result = parseTokens(tokens, 0)
  if (result.next < tokens.length) {
    throw new ParseError('Unexpected tokens after expression')
  }
  return result.node
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function toNumber(v: any): number {
  if (v === null || v === undefined) return 0
  const n = Number(v)
  return isNaN(n) ? 0 : n
}

function toString(v: any): string {
  if (v === null || v === undefined) return ''
  return String(v)
}

/**
 * Parse a date string. Returns Unix epoch (1970-01-01) for invalid dates,
 * matching Go's lispParseDate which returns zero time.Time{}.
 */
function parseDate(s: string): Date {
  const d = new Date(s)
  if (isNaN(d.getTime())) return new Date(0)
  return d
}

// ---------------------------------------------------------------------------
// Builtin function table
// ---------------------------------------------------------------------------

type BuiltinFn = (args: any[], ctx: EvalContext) => any

const BUILTINS: Record<string, BuiltinFn> = {
  '+': (args) => {
    return args.reduce((sum, a) => sum + toNumber(a), 0)
  },

  '*': (args) => {
    if (args.length === 0) return 1
    return args.reduce((prod, a) => prod * toNumber(a), 1)
  },

  '-': (args) => {
    if (args.length === 0) return 0
    if (args.length === 1) return -toNumber(args[0])
    const first = toNumber(args[0])
    return args.slice(1).reduce((acc, a) => acc - toNumber(a), first)
  },

  '/': (args) => {
    if (args.length === 0) return 1
    if (args.length === 1) return 1 / toNumber(args[0])
    const first = toNumber(args[0])
    return args.slice(1).reduce((acc, a) => acc / toNumber(a), first)
  },

  '>': (args) => {
    return toNumber(args[0]) > toNumber(args[1]) ? 1 : 0
  },

  sum: (args, ctx) => {
    const tableName = toString(args[0])
    const fieldName = toString(args[1])
    const rows = ctx.tables[tableName] || []
    return rows.reduce((sum, row) => sum + toNumber(row[fieldName]), 0)
  },

  count: (args, ctx) => {
    const tableName = toString(args[0])
    const rows = ctx.tables[tableName] || []
    return rows.length
  },

  round: (args) => {
    const val = toNumber(args[0])
    const places = toNumber(args[1])
    const factor = Math.pow(10, places)
    return Math.round(val * factor) / factor
  },

  today: () => {
    const now = new Date()
    const y = now.getFullYear()
    const m = String(now.getMonth() + 1).padStart(2, '0')
    const d = String(now.getDate()).padStart(2, '0')
    return `${y}-${m}-${d}`
  },

  datediff: (args) => {
    const d1 = parseDate(toString(args[0]))
    const d2 = parseDate(toString(args[1]))
    // datediff(a, b) = int(b - a) in days (matching Go convention).
    const diffMs = d2.getTime() - d1.getTime()
    return Math.round(diffMs / (1000 * 60 * 60 * 24))
  },

  concat: (args) => {
    return args.map((a) => toString(a)).join('')
  },

  if: (args) => {
    // Condition is truthy if toNumber != 0 (matching Go's sexpToFloat check).
    return toNumber(args[0]) !== 0 ? args[1] : args[2]
  },
}

// ---------------------------------------------------------------------------
// Evaluator
// ---------------------------------------------------------------------------

function evaluate(node: SexpNode, ctx: EvalContext): any {
  switch (node.type) {
    case 'atom': {
      if (node.valueType === 'number') return node.value
      if (node.valueType === 'string') return node.value
      // Symbol — look up in document fields
      const name = node.value as string
      if (name in ctx.fields) {
        return ctx.fields[name]
      }
      throw new Error(`Undefined symbol: ${name}`)
    }

    case 'list': {
      if (node.children.length === 0) {
        throw new Error('Empty list is not a valid expression')
      }
      const first = node.children[0]
      if (first.type !== 'atom' || first.valueType !== 'symbol') {
        throw new Error(`Expected function name, got ${first.type}`)
      }
      const fnName = first.value as string
      const fn = BUILTINS[fnName]
      if (!fn) {
        throw new Error(`Unknown function: ${fnName}`)
      }
      // Evaluate arguments, then call the builtin.
      const args = node.children.slice(1).map((child) => evaluate(child, ctx))
      return fn(args, ctx)
    }
  }
}

// ---------------------------------------------------------------------------
// Public API
// ---------------------------------------------------------------------------

/**
 * Evaluates a Lisp-style s-expression against the given document context.
 *
 * Returns:
 *   - number for arithmetic results
 *   - string for concat/today results
 *   - null on error (undefined symbol, parse error, unknown function, etc.)
 */
export function evaluateLisp(expr: string, ctx: EvalContext): number | string | null {
  if (!expr || !expr.trim()) return null

  try {
    const ast = parseExpression(expr)
    const result = evaluate(ast, ctx)
    return result === null || result === undefined ? null : result
  } catch {
    return null
  }
}
