import { useState, useCallback, useEffect, useRef } from 'react'
import { api, KoraApiError } from '@/lib/api/client'

export interface ChatMessage {
  role: 'user' | 'assistant'
  content: string
}

export interface ChatContext {
  pathname?: string
  shellMode?: string
  doctype?: string
  documentName?: string
}

interface ChatResponse {
  reply: string
  action?: string
  run_id?: string
}

interface ChatConversationResponse {
  conversation_id?: string
  messages: ChatMessage[]
  last_run_id?: string
}

export function useChat(context: ChatContext) {
  const [messages, setMessages] = useState<ChatMessage[]>([])
  const [loading, setLoading] = useState(false)
  const [restoring, setRestoring] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [errorCode, setErrorCode] = useState<string | null>(null)
  const [lastRunID, setLastRunID] = useState<string | null>(null)
  const historyRef = useRef<ChatMessage[]>([])
  const contextKey = JSON.stringify(context)

  useEffect(() => {
    let active = true
    historyRef.current = []
    setMessages([])
    setLastRunID(null)
    setRestoring(true)
    setError(null)
    setErrorCode(null)
    api.get<ChatConversationResponse>('/api/v1/ai/chat', { ...context }).then((snapshot) => {
      if (!active) return
      const restored = snapshot.messages ?? []
      historyRef.current = restored
      setMessages(restored)
      setLastRunID(snapshot.last_run_id ?? null)
    }).catch((reason) => {
      if (active) setError((reason as Error).message)
    }).finally(() => {
      if (active) setRestoring(false)
    })
    return () => { active = false }
  }, [context, contextKey])

  const send = useCallback(async (text: string, context?: ChatContext) => {
    const userMsg: ChatMessage = { role: 'user', content: text }
    const previous = historyRef.current
    const priorHistory = [...previous]
    const updated = [...priorHistory, userMsg]
    historyRef.current = updated
    setMessages(updated)
    setLoading(true)
    setError(null)
    setErrorCode(null)

    try {
      const data = await api.post<ChatResponse>('/api/v1/ai/chat', {
        message: text,
        context,
      })
      const assistantMsg: ChatMessage = { role: 'assistant', content: data.reply || 'Done.' }
      historyRef.current = [...historyRef.current, assistantMsg]
      setMessages([...historyRef.current])
      setLastRunID(data.run_id || null)
      return true
    } catch (e) {
      // Keep failed prompts retryable without duplicating them in the next request.
      historyRef.current = priorHistory
      setMessages(priorHistory)
      setError((e as Error).message)
      setErrorCode(e instanceof KoraApiError ? e.code : null)
      return false
    } finally {
      setLoading(false)
    }
  }, [])

  return { messages, loading, restoring, error, errorCode, lastRunID, send }
}
