import { useState, useMemo, useRef, useEffect, useCallback } from 'react'
import { dump, load } from 'js-yaml'
import type { DocType } from '@/types/kora'
import { Button } from '@/components/ui/button'
import { Code2, Edit3, ArrowLeftRight, AlertTriangle } from 'lucide-react'
function getCsrfToken(): string {
  const match = document.cookie.match(/(?:^|;\s*)kora_csrf=([^;]*)/)
  return match ? decodeURIComponent(match[1]) : ''
}

/** Minimal YAML syntax highlighter — wraps tokens in colored spans. */
function highlightYaml(text: string): string {
  const lines = text.split('\n')
  return lines.map((line) => {
    // Comments
    if (/^\s*#/.test(line)) {
      return `<span class="text-[#6a9955]">${esc(line)}</span>`
    }
    // Key-value line
    const m = line.match(/^(\s*)([\w_-]+)(\s*:\s*)(.*)$/)
    if (m) {
      const [, indent, key, colon, value] = m
      const indented = '&nbsp;'.repeat(indent.length)
      let valHtml: string
      if (!value || value === '') {
        valHtml = ''
      } else if (/^(true|false)$/.test(value)) {
        valHtml = `<span class="text-[#569cd6]">${esc(value)}</span>`
      } else if (/^-?\d+(\.\d+)?$/.test(value)) {
        valHtml = `<span class="text-[#b5cea8]">${esc(value)}</span>`
      } else if (/^['"].*['"]$/.test(value)) {
        valHtml = `<span class="text-[#ce9178]">${esc(value)}</span>`
      } else {
        valHtml = `<span class="text-[#ce9178]">${esc(value)}</span>`
      }
      return `${indented}<span class="text-[#9cdcfe]">${esc(key)}</span><span class="text-[#808080]">${esc(colon)}</span>${valHtml}`
    }
    // List item
    if (/^\s*-\s/.test(line)) {
      const lm = line.match(/^(\s*)(-)\s(.*)$/)
      if (lm) {
        const indent = '&nbsp;'.repeat(lm[1].length)
        return `${indent}<span class="text-[#d4d4d4]">-</span> <span class="text-[#ce9178]">${esc(lm[3])}</span>`
      }
    }
    return esc(line)
  }).join('\n')
}

function esc(s: string): string {
  return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
}

interface YamlSyntaxError {
  line: number
  column: number
  message: string
  key: string
  context: string
  detail?: string
}

interface YamlPanelProps {
  form: DocType
  onApply: (parsed: DocType) => void
}

export function YamlPanel({ form, onApply }: YamlPanelProps) {
  const [editing, setEditing] = useState(false)
  const [editText, setEditText] = useState('')
  const [applyError, setApplyError] = useState<string | null>(null)
  const [syntaxErrors, setSyntaxErrors] = useState<YamlSyntaxError[]>([])
  const [validating, setValidating] = useState(false)
  const debounceRef = useRef<ReturnType<typeof setTimeout> | null>(null)
  const textareaRef = useRef<HTMLTextAreaElement>(null)

  // Serialize form to YAML.
  const yamlText = useMemo(() => {
    try {
      return dump(form, { indent: 2, lineWidth: -1, noRefs: true })
    } catch {
      return '# Error serializing YAML'
    }
  }, [form])

  // Validate YAML against the backend with debounce.
  const validateYaml = useCallback(async (yaml: string) => {
    if (!yaml.trim()) {
      setSyntaxErrors([])
      return
    }
    setValidating(true)
    try {
      const response = await fetch('/api/system/doctype/validate', {
        method: 'POST',
        headers: { 'Content-Type': 'application/x-yaml', 'X-Kora-CSRF-Token': getCsrfToken() || '' },
        credentials: 'same-origin',
        body: yaml,
      })
      const data = await response.json()
      if (data.syntax && data.syntax.length > 0) {
        setSyntaxErrors(data.syntax)
      } else if (data.validations && data.validations.length > 0) {
        // Convert validation errors to syntax-like format for display.
        setSyntaxErrors(data.validations.map((v: any) => ({
          line: 1,
          column: 1,
          message: v.message,
          key: v.field || '',
          context: v.doctype || '',
        })))
      } else {
        setSyntaxErrors([])
      }
    } catch {
      // Network error — don't bother the user.
    } finally {
      setValidating(false)
    }
  }, [])

  const handleTextChange = (text: string) => {
    setEditText(text)
    setApplyError(null)
    // Debounce validation: 300ms after last keystroke.
    if (debounceRef.current) clearTimeout(debounceRef.current)
    debounceRef.current = setTimeout(() => validateYaml(text), 300)
  }

  // Cleanup debounce on unmount.
  useEffect(() => {
    return () => { if (debounceRef.current) clearTimeout(debounceRef.current) }
  }, [])

  // ── Auto-indentation ──────────────────────────────────────────
  const handleKeyDown = (e: React.KeyboardEvent<HTMLTextAreaElement>) => {
    const ta = e.currentTarget
    const { selectionStart, selectionEnd, value } = ta

    // Tab: insert 2 spaces (or indent selected lines).
    if (e.key === 'Tab' && !e.shiftKey) {
      e.preventDefault()
      if (selectionStart !== selectionEnd) {
        // Indent each selected line by 2 spaces.
        const before = value.substring(0, selectionStart)
        const sel = value.substring(selectionStart, selectionEnd)
        const after = value.substring(selectionEnd)
        const lines = sel.split('\n')
        const indented = lines.map((l) => '  ' + l).join('\n')
        const newValue = before + indented + after
        // Update via state (controlled component) and restore cursor.
        handleTextChange(newValue)
        requestAnimationFrame(() => {
          ta.selectionStart = selectionStart
          ta.selectionEnd = selectionStart + indented.length
        })
      } else {
        const lineStart = value.lastIndexOf('\n', selectionStart - 1) + 1
        const col = selectionStart - lineStart
        const spaces = col % 2 === 0 ? 2 : 1  // snap to even column
        const newValue = value.substring(0, selectionStart) + ' '.repeat(spaces) + value.substring(selectionEnd)
        handleTextChange(newValue)
        requestAnimationFrame(() => {
          ta.selectionStart = ta.selectionEnd = selectionStart + spaces
        })
      }
      return
    }

    // Shift+Tab: un-indent selected lines.
    if (e.key === 'Tab' && e.shiftKey) {
      e.preventDefault()
      const before = value.substring(0, selectionStart)
      const sel = value.substring(selectionStart, selectionEnd)
      const after = value.substring(selectionEnd)
      const lines = sel.split('\n')
      const unindented = lines.map((l) => l.startsWith('  ') ? l.slice(2) : l.startsWith(' ') ? l.slice(1) : l).join('\n')
      const removed = sel.length - unindented.length
      const newValue = before + unindented + after
      handleTextChange(newValue)
      requestAnimationFrame(() => {
        ta.selectionStart = selectionStart
        ta.selectionEnd = selectionEnd - removed
      })
      return
    }

    // Enter: copy indentation from current line, add extra indent after ':'.
    if (e.key === 'Enter') {
      e.preventDefault()
      const lineStart = value.lastIndexOf('\n', selectionStart - 1) + 1
      const currentLine = value.substring(lineStart, selectionStart)
      const indent = currentLine.match(/^(\s*)/)?.[1] ?? ''
      let extra = ''
      // If line ends with ':', add one more indent level.
      if (currentLine.trimEnd().endsWith(':') && currentLine.trimEnd().length > 0) {
        extra = '  '
      }
      const insertion = '\n' + indent + extra
      const newValue = value.substring(0, selectionStart) + insertion + value.substring(selectionEnd)
      handleTextChange(newValue)
      requestAnimationFrame(() => {
        ta.selectionStart = ta.selectionEnd = selectionStart + insertion.length
      })
      return
    }

    // Backspace at line start: remove one indent level (up to 2 spaces).
    if (e.key === 'Backspace' && selectionStart === selectionEnd) {
      const lineStart = value.lastIndexOf('\n', selectionStart - 1) + 1
      const col = selectionStart - lineStart
      if (col > 0 && col <= 2 && value.substring(lineStart, selectionStart).trim() === '') {
        e.preventDefault()
        const remove = col === 2 ? 2 : 1
        const newValue = value.substring(0, selectionStart - remove) + value.substring(selectionStart)
        handleTextChange(newValue)
        requestAnimationFrame(() => {
          ta.selectionStart = ta.selectionEnd = selectionStart - remove
        })
        return
      }
    }
  }

  const handleStartEdit = () => {
    setEditText(yamlText)
    setEditing(true)
    setApplyError(null)
    setSyntaxErrors([])
    // Validate initial text.
    validateYaml(yamlText)
  }

  const handleApply = async () => {
    setApplyError(null)
    try {
      const parsed = load(editText) as DocType
      const response = await fetch('/api/system/doctype/validate', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', 'X-Kora-CSRF-Token': getCsrfToken() || '' },
        credentials: 'same-origin',
        body: JSON.stringify(parsed),
      })
      if (!response.ok) {
        const err = await response.json().catch(() => ({ error: 'Validation failed' }))
        throw new Error(typeof err.error === 'string' ? err.error : err.error?.message || 'Validation failed')
      }
      onApply(parsed)
      setEditing(false)
      setSyntaxErrors([])
    } catch (e) {
      setApplyError((e as Error).message)
    }
  }

  const handleCancel = () => {
    setEditing(false)
    setApplyError(null)
    setSyntaxErrors([])
  }

  // Build set of error lines for highlighting in textarea.
  const errorLines = useMemo(() => {
    const lines = new Set<number>()
    for (const e of syntaxErrors) {
      if (e.line > 0) lines.add(e.line)
    }
    return lines
  }, [syntaxErrors])

  return (
    <div className="flex flex-col h-full">
      <div className="flex items-center justify-between px-3 py-2 border-b bg-muted/30">
        <div className="flex items-center gap-2 text-sm font-medium">
          <Code2 className="h-4 w-4" />
          YAML
          {syntaxErrors.length > 0 && (
            <span className="flex items-center gap-1 text-amber-500 text-xs">
              <AlertTriangle className="h-3 w-3" />
              {syntaxErrors.length} issue{syntaxErrors.length > 1 ? 's' : ''}
            </span>
          )}
          {validating && (
            <span className="text-muted-foreground text-xs">checking...</span>
          )}
        </div>
        {!editing ? (
          <Button variant="ghost" size="sm" onClick={handleStartEdit}>
            <Edit3 className="h-3.5 w-3.5 mr-1" /> Edit
          </Button>
        ) : (
          <div className="flex gap-1">
            <Button variant="ghost" size="sm" onClick={handleCancel}>Cancel</Button>
            <Button size="sm" onClick={handleApply}>
              <ArrowLeftRight className="h-3.5 w-3.5 mr-1" /> Apply
            </Button>
          </div>
        )}
      </div>

      {/* Error panel with line-numbered errors */}
      {syntaxErrors.length > 0 && (
        <div className="mx-2 mt-2 p-2 bg-amber-500/10 border border-amber-500/20 rounded text-xs max-h-[200px] overflow-auto">
          <div className="font-medium text-amber-600 dark:text-amber-400 mb-1 flex items-center gap-1">
            <AlertTriangle className="h-3 w-3" />
            {syntaxErrors.length} validation issue{syntaxErrors.length > 1 ? 's' : ''}
          </div>
          {syntaxErrors.map((err, i) => (
            <div key={i} className="py-0.5 text-amber-700 dark:text-amber-300 font-mono leading-relaxed">
              <span className="text-muted-foreground select-none">Line {err.line}: </span>
              {err.detail ? (
                <>
                  <span className="text-destructive">{err.message}</span>
                  {' '}<span className="text-muted-foreground">({err.detail})</span>
                </>
              ) : (
                <span>{err.message}</span>
              )}
            </div>
          ))}
        </div>
      )}

      {applyError && (
        <div className="mx-3 mt-2 p-2 text-xs text-destructive bg-destructive/10 rounded">
          {applyError}
        </div>
      )}

      {editing ? (
        <textarea
          ref={textareaRef}
          className="flex-1 w-full resize-none border-0 bg-[#1e1e1e] text-[#d4d4d4] p-3 font-mono text-xs leading-relaxed focus:outline-none focus:ring-1 focus:ring-inset focus:ring-primary"
          value={editText}
          onChange={(e) => handleTextChange(e.target.value)}
          onKeyDown={handleKeyDown}
          spellCheck={false}
        />
      ) : (
        <pre
          className="flex-1 overflow-auto m-0 p-3 text-xs font-mono leading-relaxed whitespace-pre-wrap bg-[#1e1e1e] text-[#d4d4d4]"
          dangerouslySetInnerHTML={{ __html: highlightYaml(yamlText) }}
        />
      )}
    </div>
  )
}
