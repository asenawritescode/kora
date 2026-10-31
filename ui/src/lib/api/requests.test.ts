import { afterEach, describe, expect, it, vi } from 'vitest'
import { fetchRequestHistory, fetchRequests } from './requests'

const originalFetch = globalThis.fetch

afterEach(() => {
  globalThis.fetch = originalFetch
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
})

describe('embedded request pagination transport', () => {
  it('forwards inbox and history cursors to the paginated request endpoints', async () => {
    vi.stubGlobal('window', { location: { origin: 'http://localhost', pathname: '/s/acme/workspace' } })
    vi.stubGlobal('document', { getElementById: () => null, cookie: '' })
    const fetchSpy = vi.fn(async (_input: RequestInfo | URL, _init?: RequestInit) => {
      expect(_input).toBeDefined()
      expect(_init?.credentials).toBe('same-origin')
      return new Response(JSON.stringify({ data: { items: [], next_cursor: 'next' } }), { status: 200 })
    })
    vi.stubGlobal('fetch', fetchSpy)

    await fetchRequests({ view: 'completed', cursor: 'request-cursor-25' })
    await fetchRequestHistory('01K7REQUEST00000000000000000', { limit: 50, cursor: 'history-cursor-50' })

    expect(String(fetchSpy.mock.calls[0]?.[0])).toContain('cursor=request-cursor-25')
    expect(String(fetchSpy.mock.calls[1]?.[0])).toContain('cursor=history-cursor-50')
  })
})
