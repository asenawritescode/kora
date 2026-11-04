import { renderToStaticMarkup } from 'react-dom/server'
import { beforeEach, describe, expect, it, vi } from 'vitest'

const useQueryMock = vi.hoisted(() => vi.fn())

vi.mock('@tanstack/react-query', () => ({ useQuery: useQueryMock }))
vi.mock('@tanstack/react-router', () => ({ useNavigate: () => vi.fn() }))
vi.mock('@/lib/api/system', () => ({ fetchNavigation: vi.fn(), navigationQueryKey: () => ['navigation'] }))
vi.mock('@/lib/basepath', () => ({ getSitePrefix: () => '' }))

import RequestsInboxPage from './index'

describe('embedded request inbox pagination', () => {
  beforeEach(() => {
    useQueryMock.mockReset()
    useQueryMock
      .mockReturnValueOnce({ data: { supported_capabilities: ['requests.inbox.v2'], user: { name: 'alice' } } })
      .mockReturnValueOnce({ isLoading: false, isFetching: false, isError: false, data: { items: [], next_cursor: 'cursor-25' } })
  })

  it('exposes previous and next request pages from the server cursor', () => {
    const markup = renderToStaticMarkup(<RequestsInboxPage />)

    expect(markup).toContain('aria-label="Request inbox pages"')
    expect(markup).toMatch(/disabled=""[^>]*>Previous page/)
    expect(markup).not.toMatch(/disabled=""[^>]*>Next page/)
  })
})
