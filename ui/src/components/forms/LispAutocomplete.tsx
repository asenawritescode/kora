import { useState, useRef, useEffect } from 'react'

interface LispFunction {
  name: string
  signature: string
  desc: string
}

const LISP_FUNCTIONS: LispFunction[] = [
  { name: '+', signature: '(+ a b ...)', desc: 'Addition' },
  { name: '-', signature: '(- a b)', desc: 'Subtraction' },
  { name: '*', signature: '(* a b ...)', desc: 'Multiplication' },
  { name: '/', signature: '(/ a b)', desc: 'Division' },
  { name: 'sum', signature: '(sum "table" "field")', desc: 'Sum field across child table rows' },
  { name: 'count', signature: '(count "table")', desc: 'Count rows in child table' },
  { name: 'round', signature: '(round value decimals)', desc: 'Round to N decimal places' },
  { name: 'today', signature: '(today)', desc: 'Current date as YYYY-MM-DD' },
  { name: 'datediff', signature: '(datediff date1 date2)', desc: 'Days between two dates' },
  { name: 'concat', signature: '(concat a b ...)', desc: 'Concatenate strings' },
  { name: 'if', signature: '(if cond then else)', desc: 'Conditional evaluation' },
  { name: '>', signature: '(> a b)', desc: 'Greater than (returns 1 or 0)' },
]

interface Props {
  value: string
  onChange: (value: string) => void
  fieldNames?: string[]
  className?: string
  placeholder?: string
}

export function LispAutocomplete({ value, onChange, fieldNames = [], className, placeholder }: Props) {
  const [showSuggestions, setShowSuggestions] = useState(false)
  const [filter, setFilter] = useState('')
  const [selectedIdx, setSelectedIdx] = useState(0)
  const inputRef = useRef<HTMLInputElement>(null)

  // Determine what the user might be typing based on cursor position.
  const getCurrentToken = (): { token: string; isAfterParen: boolean } => {
    if (!inputRef.current) return { token: '', isAfterParen: false }
    const pos = inputRef.current.selectionStart || 0
    const text = value.slice(0, pos)
    const lastParen = text.lastIndexOf('(')
    if (lastParen < 0) return { token: '', isAfterParen: false }
    const afterParen = text.slice(lastParen + 1)
    // Find the start of the current word (working backwards from cursor).
    const words = afterParen.split(/\s+/)
    const currentWord = words[words.length - 1] || ''
    return { token: currentWord, isAfterParen: true }
  }

  // Get suggestions based on context.
  const getSuggestions = (): LispFunction[] => {
    const { token, isAfterParen } = getCurrentToken()
    if (!isAfterParen) return []
    // If token is empty or just a parenthesis, show all functions.
    if (!token) return LISP_FUNCTIONS
    // Filter functions matching the token.
    return LISP_FUNCTIONS.filter((f) => f.name.startsWith(token))
  }

  const suggestions = getSuggestions()

  const insertSuggestion = (fn: LispFunction) => {
    if (!inputRef.current) return
    const pos = inputRef.current.selectionStart || 0
    const text = value
    // Find the last '(' before cursor and replace the partial word.
    const before = text.slice(0, pos)
    const after = text.slice(pos)
    const lastParen = before.lastIndexOf('(')
    const prefix = before.slice(0, lastParen + 1)
    // Replace everything from after-paren to cursor with the function name.
    const replacement = fn.name + ' '
    const newValue = prefix + replacement + after
    onChange(newValue)
    setShowSuggestions(false)
    // Set cursor after the inserted function name.
    setTimeout(() => {
      if (inputRef.current) {
        const newPos = prefix.length + replacement.length
        inputRef.current.selectionStart = newPos
        inputRef.current.selectionEnd = newPos
        inputRef.current.focus()
      }
    }, 0)
  }

  const handleKeyDown = (e: React.KeyboardEvent) => {
    if (!showSuggestions || suggestions.length === 0) {
      if (e.key === '(') setShowSuggestions(true)
      return
    }
    if (e.key === 'ArrowDown') {
      e.preventDefault()
      setSelectedIdx((i) => (i + 1) % suggestions.length)
    } else if (e.key === 'ArrowUp') {
      e.preventDefault()
      setSelectedIdx((i) => (i - 1 + suggestions.length) % suggestions.length)
    } else if (e.key === 'Enter' || e.key === 'Tab') {
      e.preventDefault()
      insertSuggestion(suggestions[selectedIdx])
    } else if (e.key === 'Escape') {
      setShowSuggestions(false)
    }
  }

  // Reset selection when suggestions change.
  useEffect(() => {
    setSelectedIdx(0)
  }, [filter])

  return (
    <div className="relative">
      <input
        ref={inputRef}
        type="text"
        value={value}
        onChange={(e) => {
          onChange(e.target.value)
          if (e.target.value.endsWith('(')) setShowSuggestions(true)
        }}
        onFocus={() => { if (value.includes('(')) setShowSuggestions(true) }}
        onBlur={() => setTimeout(() => setShowSuggestions(false), 200)}
        onKeyDown={handleKeyDown}
        className={className}
        placeholder={placeholder}
        style={{ fontFamily: 'monospace' }}
      />
      {showSuggestions && suggestions.length > 0 && (
        <div className="absolute z-50 mt-1 w-80 bg-popover border rounded-md shadow-md text-sm">
          {suggestions.map((fn, i) => (
            <div
              key={fn.name}
              className={`px-3 py-1.5 cursor-pointer flex justify-between items-center ${
                i === selectedIdx ? 'bg-accent text-accent-foreground' : 'hover:bg-muted'
              }`}
              onMouseDown={(e) => {
                e.preventDefault()
                insertSuggestion(fn)
              }}
            >
              <span className="font-mono font-medium">{fn.name}</span>
              <span className="text-xs text-muted-foreground ml-2 truncate">{fn.signature}</span>
            </div>
          ))}
        </div>
      )}
    </div>
  )
}
