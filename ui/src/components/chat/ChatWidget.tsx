import { useState, useMemo } from 'react'
import { useChat } from './useChat'
import { MessageCircle, X, Send, Loader2 } from 'lucide-react'

/** Simple markdown-to-HTML for tables, bold, and basic formatting. */
function renderMarkdown(text: string): string {
  let html = text
  // Bold.
  html = html.replace(/\*\*(.+?)\*\*/g, '<strong>$1</strong>')
  // Italic.
  html = html.replace(/\*(.+?)\*/g, '<em>$1</em>')
  // Inline code.
  html = html.replace(/`(.+?)`/g, '<code class="bg-muted px-1 rounded text-xs">$1</code>')
  // Tables: split into lines, detect | separator, wrap in HTML table.
  const lines = html.split('\n')
  let inTable = false
  let result: string[] = []
  for (const line of lines) {
    const trimmed = line.trim()
    if (trimmed.startsWith('|') && trimmed.endsWith('|')) {
      if (!inTable) {
        result.push('<table class="w-full text-xs border-collapse my-2"><tbody>')
        inTable = true
      }
      const cells = trimmed.split('|').slice(1, -1).map(c => c.trim())
      const isHeader = cells.every(c => c === '---' || c === ':---' || c === '---:' || c === ':---:')
      if (isHeader) continue
      const tag = inTable && result.filter(l => l.includes('<tr>')).length === 0 ? 'th' : 'td'
      const cellHtml = cells.map(c => `<${tag} class="border px-2 py-1">${c}</${tag}>`).join('')
      result.push(`<tr>${cellHtml}</tr>`)
    } else {
      if (inTable) {
        result.push('</tbody></table>')
        inTable = false
      }
      if (trimmed) {
        result.push(`<p class="my-1">${trimmed}</p>`)
      } else {
        result.push('<br/>')
      }
    }
  }
  if (inTable) result.push('</tbody></table>')
  return result.join('\n')
}

export function ChatWidget() {
  const [open, setOpen] = useState(false)
  const [input, setInput] = useState('')
  const { messages, loading, error, send } = useChat()

  const handleSend = async () => {
    const text = input.trim()
    if (!text || loading) return
    setInput('')
    await send(text)
  }

  const handleKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault()
      handleSend()
    }
  }

  return (
    <>
      {/* Floating button */}
      {!open && (
        <button
          onClick={() => setOpen(true)}
          className="fixed bottom-6 right-6 z-50 flex h-14 w-14 items-center justify-center rounded-full bg-primary text-primary-foreground shadow-lg hover:bg-primary/90 transition-all"
          aria-label="Open AI chat"
        >
          <MessageCircle className="h-6 w-6" />
        </button>
      )}

      {/* Chat panel */}
      {open && (
        <div className="fixed bottom-6 right-6 z-50 flex h-[520px] w-[380px] max-w-[calc(100vw-2rem)] flex-col rounded-xl border bg-background shadow-2xl">
          {/* Header */}
          <div className="flex items-center justify-between px-4 py-3 border-b">
            <div>
              <h3 className="font-semibold text-sm">AI Assistant</h3>
              <p className="text-xs text-muted-foreground">Ask me anything about your data</p>
            </div>
            <button
              onClick={() => setOpen(false)}
              className="rounded-md p-1 hover:bg-muted"
              aria-label="Close chat"
            >
              <X className="h-4 w-4" />
            </button>
          </div>

          {/* Messages */}
          <div className="flex-1 overflow-y-auto px-4 py-3 space-y-3">
            {messages.length === 0 && !loading && (
              <div className="flex h-full items-center justify-center text-sm text-muted-foreground">
                <p>Ask me to create, find, or update data...</p>
              </div>
            )}
            {messages.map((msg, i) => (
              <div
                key={i}
                className={`flex ${msg.role === 'user' ? 'justify-end' : 'justify-start'}`}
              >
                <div
                  className={`max-w-[85%] rounded-lg px-3 py-2 text-sm ${
                    msg.role === 'user'
                      ? 'bg-primary text-primary-foreground'
                      : 'bg-muted'
                  }`}
                >
                  {msg.role === 'assistant' ? (
                    <div
                      className="[&_table]:w-full [&_table]:border-collapse [&_th]:border [&_th]:border-muted-foreground/20 [&_th]:px-2 [&_th]:py-1 [&_th]:text-left [&_th]:text-xs [&_th]:font-semibold [&_td]:border [&_td]:border-muted-foreground/20 [&_td]:px-2 [&_td]:py-1 [&_td]:text-xs [&_p]:my-1 [&_strong]:text-foreground"
                      dangerouslySetInnerHTML={{ __html: renderMarkdown(msg.content) }}
                    />
                  ) : (
                    msg.content
                  )}
                </div>
              </div>
            ))}
            {loading && (
              <div className="flex justify-start">
                <div className="rounded-lg bg-muted px-3 py-2 text-sm flex items-center gap-2">
                  <Loader2 className="h-3 w-3 animate-spin" />
                  Thinking...
                </div>
              </div>
            )}
            {error && (
              <div className="rounded-lg bg-destructive/10 px-3 py-2 text-sm text-destructive">
                {error}
              </div>
            )}
          </div>

          {/* Input */}
          <div className="border-t px-4 py-3">
            <div className="flex gap-2">
              <input
                type="text"
                value={input}
                onChange={(e) => setInput(e.target.value)}
                onKeyDown={handleKeyDown}
                placeholder="Ask something..."
                disabled={loading}
                className="flex-1 rounded-md border bg-background px-3 py-2 text-sm focus:outline-none focus:ring-1 focus:ring-primary"
              />
              <button
                onClick={handleSend}
                disabled={loading || !input.trim()}
                className="rounded-md bg-primary px-3 py-2 text-primary-foreground hover:bg-primary/90 disabled:opacity-50"
              >
                <Send className="h-4 w-4" />
              </button>
            </div>
          </div>
        </div>
      )}
    </>
  )
}
