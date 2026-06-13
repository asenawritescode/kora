import { useState, useMemo } from 'react'
import { dump, load } from 'js-yaml'
import type { DocType } from '@/types/kora'
import { Button } from '@/components/ui/button'
import { Code2, Edit3, ArrowLeftRight } from 'lucide-react'

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

interface YamlPanelProps {
  form: DocType
  onApply: (parsed: DocType) => void
}

export function YamlPanel({ form, onApply }: YamlPanelProps) {
  const [editing, setEditing] = useState(false)
  const [editText, setEditText] = useState('')
  const [applyError, setApplyError] = useState<string | null>(null)

  // Serialize form to YAML.
  const yamlText = useMemo(() => {
    try {
      return dump(form, { indent: 2, lineWidth: -1, noRefs: true })
    } catch {
      return '# Error serializing YAML'
    }
  }, [form])

  const handleStartEdit = () => {
    setEditText(yamlText)
    setEditing(true)
    setApplyError(null)
  }

  const handleApply = async () => {
    setApplyError(null)
    try {
      // Parse YAML client-side.
      const parsed = load(editText) as DocType
      // Validate against backend (optional, catches structural issues).
      const response = await fetch('/api/system/doctype/validate', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        credentials: 'same-origin',
        body: JSON.stringify(parsed),
      })
      if (!response.ok) {
        const err = await response.json().catch(() => ({ error: 'Validation failed' }))
        throw new Error(typeof err.error === 'string' ? err.error : err.error?.message || 'Validation failed')
      }
      onApply(parsed)
      setEditing(false)
    } catch (e) {
      setApplyError((e as Error).message)
    }
  }

  const handleCancel = () => {
    setEditing(false)
    setApplyError(null)
  }

  return (
    <div className="flex flex-col h-full">
      <div className="flex items-center justify-between px-3 py-2 border-b bg-muted/30">
        <div className="flex items-center gap-2 text-sm font-medium">
          <Code2 className="h-4 w-4" />
          YAML
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

      {applyError && (
        <div className="mx-3 mt-2 p-2 text-xs text-destructive bg-destructive/10 rounded">
          {applyError}
        </div>
      )}

      {editing ? (
        <textarea
          className="flex-1 w-full resize-none border-0 bg-[#1e1e1e] text-[#d4d4d4] p-3 font-mono text-xs leading-relaxed focus:outline-none focus:ring-1 focus:ring-inset focus:ring-primary"
          value={editText}
          onChange={(e) => setEditText(e.target.value)}
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
