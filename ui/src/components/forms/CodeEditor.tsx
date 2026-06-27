import { useEffect, useRef } from 'react'
import { EditorView, keymap, lineNumbers, highlightActiveLine, highlightSpecialChars, drawSelection, rectangularSelection } from '@codemirror/view'
import { EditorState } from '@codemirror/state'
import { javascript } from '@codemirror/lang-javascript'
import { oneDark } from '@codemirror/theme-one-dark'
import { defaultKeymap, history, historyKeymap, indentWithTab } from '@codemirror/commands'
import { bracketMatching, indentOnInput } from '@codemirror/language'
import { closeBrackets } from '@codemirror/autocomplete'

interface CodeEditorProps {
  value: string
  onChange: (value: string) => void
  readOnly?: boolean
  minHeight?: string
}

export function CodeEditor({ value, onChange, readOnly = false, minHeight = '300px' }: CodeEditorProps) {
  const containerRef = useRef<HTMLDivElement>(null)
  const viewRef = useRef<EditorView | null>(null)
  const isUpdatingFromProp = useRef(false)

  useEffect(() => {
    if (!containerRef.current) return

    const updateListener = EditorView.updateListener.of((update) => {
      if (update.docChanged && !isUpdatingFromProp.current) {
        const newValue = update.state.doc.toString()
        onChange(newValue)
      }
    })

    const state = EditorState.create({
      doc: value,
      extensions: [
        lineNumbers(),
        highlightActiveLine(),
        highlightSpecialChars(),
        drawSelection(),
        rectangularSelection(),
        bracketMatching(),
        closeBrackets(),
        indentOnInput(),
        oneDark,
        javascript(),
        history(),
        keymap.of([
          ...defaultKeymap,
          ...historyKeymap,
          indentWithTab,
        ]),
        updateListener,
        EditorView.editable.of(!readOnly),
        EditorView.theme({
          '&': {
            height: '100%',
            minHeight: minHeight,
          },
          '.cm-scroller': {
            overflow: 'auto',
            fontFamily: 'var(--font-mono, "Geist Mono", monospace)',
            fontSize: '13px',
            lineHeight: '1.6',
          },
          '.cm-content': {
            padding: '12px',
          },
          '.cm-gutters': {
            borderRight: '1px solid #333',
            backgroundColor: '#1a1a1a',
            color: '#858585',
          },
          '.cm-activeLineGutter': {
            backgroundColor: '#2a2a2a',
          },
        }),
      ],
    })

    const view = new EditorView({
      state,
      parent: containerRef.current,
    })

    viewRef.current = view

    return () => {
      view.destroy()
      viewRef.current = null
    }
  }, []) // eslint-disable-line react-hooks/exhaustive-deps

  // Sync external value changes into the editor.
  useEffect(() => {
    const view = viewRef.current
    if (!view) return

    const currentDoc = view.state.doc.toString()
    if (value !== currentDoc) {
      isUpdatingFromProp.current = true
      view.dispatch({
        changes: { from: 0, to: currentDoc.length, insert: value },
      })
      isUpdatingFromProp.current = false
    }
  }, [value])

  return (
    <div
      ref={containerRef}
      className="w-full border rounded-md overflow-hidden bg-[#1e1e1e]"
      style={{ minHeight }}
    />
  )
}
