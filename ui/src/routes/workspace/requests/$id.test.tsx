import { renderToStaticMarkup } from 'react-dom/server'
import { beforeEach, describe, expect, it, vi } from 'vitest'

const useQueryMock = vi.hoisted(() => vi.fn())

vi.mock('@tanstack/react-query', () => ({
  useQuery: useQueryMock,
  useQueryClient: () => ({ invalidateQueries: vi.fn() }),
}))
vi.mock('@tanstack/react-router', () => ({ useNavigate: () => vi.fn(), useParams: () => ({ id: 'req-1' }) }))
vi.mock('@/lib/api/system', () => ({ fetchNavigation: vi.fn(), navigationQueryKey: () => ['navigation'] }))
vi.mock('@/lib/basepath', () => ({ getSitePrefix: () => '' }))
vi.mock('@/lib/api/requests', () => ({
  fetchRequest: vi.fn(),
  fetchRequestHistory: vi.fn(),
  idempotencyKey: () => 'stable-key',
  markRequestRead: vi.fn(),
  respondToRequest: vi.fn(),
}))

import RequestDetailPage from './$id'

describe('embedded request detail pagination', () => {
  beforeEach(() => {
    useQueryMock.mockReset()
    useQueryMock
      .mockReturnValueOnce({ data: { supported_capabilities: ['requests.inbox.v2'], user: { name: 'alice' } } })
      .mockReturnValueOnce({ data: { id: 'req-1', title: 'Task', status: 'pending', assignments: [], allowed_responses: [] }, isLoading: false, isError: false, refetch: vi.fn() })
      .mockReturnValueOnce({ data: { items: [], next_cursor: 'cursor-50' }, isLoading: false, isFetching: false, isError: false, refetch: vi.fn() })
  })

  it('exposes earlier and later assignment-history pages', () => {
    const markup = renderToStaticMarkup(<RequestDetailPage />)

    expect(markup).toContain('aria-label="Assignment history pages"')
    expect(markup).toMatch(/disabled=""[^>]*>Earlier history/)
    expect(markup).not.toMatch(/disabled=""[^>]*>Later history/)
  })
})
