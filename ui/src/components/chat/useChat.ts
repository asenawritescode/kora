import { useState, useCallback, useRef } from 'react'

function getCsrfToken(): string {
  const match = document.cookie.match(/(?:^|;\s*)kora_csrf=([^;]*)/)
  return match ? decodeURIComponent(match[1]) : ''
}

export interface ChatMessage {
  role: 'user' | 'assistant'
  content: string
}

export function useChat() {
  const [messages, setMessages] = useState<ChatMessage[]>([])
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const historyRef = useRef<ChatMessage[]>([])

  const send = useCallback(async (text: string) => {
    const userMsg: ChatMessage = { role: 'user', content: text }
    const updated = [...historyRef.current, userMsg]
    historyRef.current = updated
    setMessages([...updated])
    setLoading(true)
    setError(null)

    try {
      const resp = await fetch('/api/v1/chat', {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          'X-Kora-CSRF-Token': getCsrfToken() || '',
        },
        credentials: 'same-origin',
        body: JSON.stringify({
          message: text,
          history: historyRef.current.slice(0, -1), // exclude the just-added user message
        }),
      })

      const data = await resp.json()
      if (!resp.ok) {
        throw new Error(data.error?.message || data.error || 'Chat failed')
      }

      const assistantMsg: ChatMessage = { role: 'assistant', content: data.reply || 'Done.' }
      historyRef.current = [...historyRef.current, assistantMsg]
      setMessages([...historyRef.current])
    } catch (e) {
      setError((e as Error).message)
    } finally {
      setLoading(false)
    }
  }, [])

  return { messages, loading, error, send }
}
