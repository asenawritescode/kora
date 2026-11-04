import { useMemo, useState, useRef, useEffect, type KeyboardEvent, type ReactNode } from 'react'
import { Link, useRouterState } from '@tanstack/react-router'
import { useUIStore } from '@/lib/ui-store'
import { useChat } from './useChat'
import { MessageCircle, X, Send, Loader2, Sparkles, Wand2, Search, FilePlus2 } from 'lucide-react'
import ReactMarkdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import { cn } from '@/lib/utils'

type Suggestion = {
  label: string
  icon: ReactNode
  prompt: string
}

export function ChatWidget() {
  const [open, setOpen] = useState(false)
  const [input, setInput] = useState('')
  const routerState = useRouterState()
  const { shellMode } = useUIStore()
  const messagesEndRef = useRef<HTMLDivElement>(null)

  const context = useMemo(() => {
    const pathname = routerState.location.pathname
    const parts = pathname.split('/').filter(Boolean)
    const doctype = parts[1] && parts[1] !== 'admin' ? decodeURIComponent(parts[1]) : undefined
    const documentName = parts[2] && parts[2] !== 'new' && parts[1] !== 'admin' ? decodeURIComponent(parts[2]) : undefined
    return {
      pathname,
      shellMode,
      doctype: pathname.startsWith('/workspace/') && !pathname.startsWith('/workspace/admin')
        ? doctype
        : undefined,
      documentName,
    }
  }, [routerState.location.pathname, shellMode])
  const { messages, loading, restoring, error, errorCode, lastRunID, send } = useChat(context)

  // Auto-scroll to bottom when new messages arrive or loading state changes.
  useEffect(() => {
    messagesEndRef.current?.scrollIntoView({ behavior: 'smooth' })
  }, [messages, loading])

  const suggestions = useMemo(() => {
    const base: Suggestion[] = [
      { label: 'Summarize page', icon: <Sparkles className="h-3.5 w-3.5" />, prompt: 'Summarize what I should focus on here.' },
      { label: 'Find records', icon: <Search className="h-3.5 w-3.5" />, prompt: 'Help me find the records I need.' },
      { label: 'Draft change', icon: <FilePlus2 className="h-3.5 w-3.5" />, prompt: 'Help me draft the next change I should make.' },
    ]

    if (context.doctype) {
      return [
        { label: 'Review this doc', icon: <Wand2 className="h-3.5 w-3.5" />, prompt: `Review the current ${context.doctype} page and suggest the next best action.` },
        { label: 'Create record', icon: <FilePlus2 className="h-3.5 w-3.5" />, prompt: `Help me create a new ${context.doctype} record.` },
        { label: 'Search this type', icon: <Search className="h-3.5 w-3.5" />, prompt: `Help me find ${context.doctype} records that match my needs.` },
      ]
    }

    if (context.pathname.startsWith('/workspace/admin')) {
      return [
        { label: 'Explain admin page', icon: <Sparkles className="h-3.5 w-3.5" />, prompt: 'Explain what I should do on this admin page.' },
        { label: 'Check settings', icon: <Wand2 className="h-3.5 w-3.5" />, prompt: 'Review the current admin settings and point out anything important.' },
        { label: 'Find config issue', icon: <Search className="h-3.5 w-3.5" />, prompt: 'Help me find a configuration issue or missing setting.' },
      ]
    }

    return base
  }, [context.doctype, context.pathname])

  const handleSend = async (textOverride?: string) => {
    const text = (textOverride ?? input).trim()
    if (!text || loading || restoring) return
    const sent = await send(text, context)
    if (sent && !textOverride) setInput('')
  }

  const handleKeyDown = (e: KeyboardEvent<HTMLInputElement>) => {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault()
      handleSend()
    }
  }

  return (
    <>
      {!open && (
        <button
          onClick={() => setOpen(true)}
          className="fixed bottom-6 right-6 z-50 flex h-14 w-14 items-center justify-center rounded-full bg-primary text-primary-foreground shadow-lg transition-all hover:bg-primary/90"
          aria-label="Open AI chat"
        >
          <MessageCircle className="h-6 w-6" />
        </button>
      )}

      {open && (
        <div data-ai-run-id={lastRunID ?? undefined} className="fixed bottom-6 right-6 z-50 flex h-[560px] w-[400px] max-w-[calc(100vw-2rem)] flex-col overflow-hidden rounded-2xl border bg-background shadow-2xl">
          <div className="border-b bg-gradient-to-r from-primary/10 to-transparent px-4 py-3">
            <div className="flex items-start justify-between gap-3">
              <div className="min-w-0">
                <div className="flex items-center gap-2">
                  <span className="inline-flex h-7 w-7 items-center justify-center rounded-full bg-primary/10 text-primary">
                    <Sparkles className="h-4 w-4" />
                  </span>
                  <h3 className="text-sm font-semibold">AI Co-creator</h3>
                </div>
                <p className="mt-1 text-xs text-muted-foreground">
                  Uses the current page context to help you act faster.
                </p>
              </div>
              <button
                onClick={() => setOpen(false)}
                className="rounded-md p-1 hover:bg-muted"
                aria-label="Close chat"
              >
                <X className="h-4 w-4" />
              </button>
            </div>

            <div className="mt-3 flex flex-wrap gap-2">
              {suggestions.slice(0, 3).map((s) => (
                <button
                  key={s.label}
                  type="button"
                  onClick={() => handleSend(s.prompt)}
                  disabled={loading || restoring}
                  className="inline-flex items-center gap-1.5 rounded-full border bg-background px-3 py-1.5 text-xs font-medium text-foreground transition-colors hover:bg-muted disabled:opacity-50"
                >
                  {s.icon}
                  {s.label}
                </button>
              ))}
            </div>
          </div>

          <div className="flex-1 space-y-3 overflow-y-auto px-4 py-3">
            {messages.length === 0 && !loading && !restoring && (
              <div className="rounded-xl border border-dashed bg-muted/20 px-4 py-5 text-sm text-muted-foreground">
                Ask me to summarize this page, find records, or draft the next action.
              </div>
            )}
            {restoring && messages.length === 0 && (
              <div className="text-center text-xs text-muted-foreground">Loading this conversation…</div>
            )}
            {messages.map((msg, i) => (
              <div
                key={i}
                className={cn('flex', msg.role === 'user' ? 'justify-end' : 'justify-start')}
              >
                <div
                  className={cn(
                    'max-w-[90%] rounded-2xl px-3 py-2.5 text-sm shadow-sm',
                    msg.role === 'user'
                      ? 'bg-primary text-primary-foreground'
                      : 'border bg-muted/40 text-foreground',
                  )}
                >
                  {msg.role === 'assistant' ? (
                    <ReactMarkdown
                      remarkPlugins={[remarkGfm]}
                      components={{
                        p: ({ children }) => <p className="mb-2 last:mb-0 leading-6">{children}</p>,
                        strong: ({ children }) => <strong className="font-semibold text-foreground">{children}</strong>,
                        em: ({ children }) => <em className="italic">{children}</em>,
                        ul: ({ children }) => <ul className="my-2 list-disc space-y-1 pl-5">{children}</ul>,
                        ol: ({ children }) => <ol className="my-2 list-decimal space-y-1 pl-5">{children}</ol>,
                        li: ({ children }) => <li className="leading-6">{children}</li>,
                        a: ({ href, children }) => (
                          <a href={href} target="_blank" rel="noreferrer" className="text-primary underline underline-offset-4">
                            {children}
                          </a>
                        ),
                        code: ({ inline, className, children, ...props }: any) =>
                          inline ? (
                            <code className="rounded bg-muted px-1 py-0.5 font-mono text-[0.85em] text-foreground" {...props}>
                              {children}
                            </code>
                          ) : (
                            <code className={cn('block overflow-x-auto rounded-lg bg-slate-950 px-3 py-2 font-mono text-xs text-slate-50', className)} {...props}>
                              {children}
                            </code>
                          ),
                        pre: ({ children }) => <pre className="my-2 overflow-x-auto rounded-lg bg-slate-950 p-0 text-slate-50">{children}</pre>,
                        blockquote: ({ children }) => <blockquote className="my-2 border-l-2 border-primary/40 pl-4 italic text-muted-foreground">{children}</blockquote>,
                        table: ({ children }) => (
                          <div className="my-3 overflow-x-auto rounded-lg border">
                            <table className="w-full border-collapse text-sm">{children}</table>
                          </div>
                        ),
                        thead: ({ children }) => <thead className="bg-muted/50">{children}</thead>,
                        th: ({ children }) => <th className="border-b px-3 py-2 text-left font-semibold">{children}</th>,
                        td: ({ children }) => <td className="border-b px-3 py-2 align-top">{children}</td>,
                        hr: () => <hr className="my-3 border-border" />,
                      }}
                    >
                      {msg.content}
                    </ReactMarkdown>
                  ) : (
                    msg.content
                  )}
                </div>
              </div>
            ))}
            {loading && (
              <div className="flex justify-start">
                <div className="flex items-center gap-2 rounded-2xl border bg-muted/40 px-3 py-2 text-sm">
                  <Loader2 className="h-3.5 w-3.5 animate-spin" />
                  Thinking...
                </div>
              </div>
            )}
            {error && (
              <div className="rounded-xl border border-destructive/20 bg-destructive/10 px-3 py-2 text-sm text-destructive">
                {error}
                {errorCode === 'ai.provider_not_configured' && (
                  <Link to="/workspace/admin/secrets" className="ml-1 font-medium underline underline-offset-2">
                    Configure a provider
                  </Link>
                )}
              </div>
            )}
            <div ref={messagesEndRef} />
          </div>

          <div className="border-t bg-background px-4 py-3">
            <div className="mb-2 flex items-center justify-between">
              <span className="text-[11px] font-medium uppercase tracking-wide text-muted-foreground">
                Context-aware
              </span>
              <span className="rounded-full bg-muted px-2 py-0.5 text-[11px] text-muted-foreground">
                {context.pathname}
              </span>
            </div>
            <div className="flex gap-2">
              <input
                type="text"
                value={input}
                onChange={(e) => setInput(e.target.value)}
                onKeyDown={handleKeyDown}
                placeholder={context.doctype ? `Ask about ${context.doctype}...` : 'Ask something...'}
                disabled={loading || restoring}
                className="flex-1 rounded-xl border bg-background px-3 py-2.5 text-sm focus:outline-none focus:ring-1 focus:ring-primary"
              />
              <button
                onClick={() => handleSend()}
                disabled={loading || restoring || !input.trim()}
                className="inline-flex items-center justify-center rounded-xl bg-primary px-3.5 py-2.5 text-primary-foreground hover:bg-primary/90 disabled:opacity-50"
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
