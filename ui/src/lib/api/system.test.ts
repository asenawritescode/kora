import { describe, expect, it, vi } from 'vitest'

vi.mock('./client', () => ({
  api: {
    get: vi.fn(),
    post: vi.fn(),
    operation: vi.fn(),
    put: vi.fn(),
    delete: vi.fn(),
  },
}))

import {
  fetchUserDirectory,
  fetchUserDirectoryByNames,
  navigationQueryKey,
  fetchRollbackVersionPreview,
  rollbackVersion,
  isImmutableConfigVersion,
  selectRollbackTargetVersion,
} from './system'

describe('user directory', () => {
  it('uses the bounded tenant user-directory endpoint with the visible search term', async () => {
    const { api } = await import('./client')
    vi.mocked(api.get).mockResolvedValueOnce([])

    await fetchUserDirectory({ search: 'Alice Example', limit: 25 })

    expect(api.get).toHaveBeenCalledWith('/api/v1/system/user-directory', { q: 'Alice Example', limit: 25 })
  })

  it('resolves visible user labels for a bounded set of record rows in one request', async () => {
    const { api } = await import('./client')
    vi.mocked(api.get).mockResolvedValueOnce([])

    await fetchUserDirectoryByNames(['user-1', 'user-2'])

    expect(api.get).toHaveBeenCalledWith('/api/v1/system/user-directory', { names: 'user-1,user-2' })
  })
})

describe('config version rollback helpers', () => {
  it('scopes navigation cache keys by path-routed tenant', () => {
    vi.stubGlobal('window', { location: { pathname: '/s/acme/workspace', host: 'kora.test' } })
    expect(navigationQueryKey()).toEqual(['navigation', '/s/acme'])
    vi.unstubAllGlobals()
  })
  it('scopes navigation cache keys by host-routed tenant', () => {
    vi.stubGlobal('window', { location: { pathname: '/workspace', host: 'acme.kora.test' } })
    expect(navigationQueryKey()).toEqual(['navigation', 'acme.kora.test'])
  })
  it('treats active and superseded versions as immutable', () => {
    expect(isImmutableConfigVersion({ status: 'Active' })).toBe(true)
    expect(isImmutableConfigVersion({ status: 'Superseded' })).toBe(true)
    expect(isImmutableConfigVersion({ status: 'Draft' })).toBe(false)
  })

  it('selects the newest immutable version for rollback', () => {
    const target = selectRollbackTargetVersion([
      { id: '1', site: 's', version: 1, created_at: '2026-08-13T00:00:00Z', created_by: 'a', label: 'v1', status: 'Draft' },
      { id: '2', site: 's', version: 2, created_at: '2026-08-13T00:00:00Z', created_by: 'a', label: 'v2', status: 'Active' },
      { id: '3', site: 's', version: 3, created_at: '2026-08-13T00:00:00Z', created_by: 'a', label: 'v3', status: 'Superseded' },
    ])

    expect(target?.id).toBe('3')
  })

  it('returns null when only drafts exist', () => {
    expect(selectRollbackTargetVersion([
      { id: '1', site: 's', version: 1, created_at: '2026-08-13T00:00:00Z', created_by: 'a', label: 'v1', status: 'Draft' },
    ])).toBeNull()
  })

  it('builds rollback preview and rollback endpoints for config versions', async () => {
    const { api } = await import('./client')
    vi.mocked(api.get).mockResolvedValueOnce({ version_id: '3', status: 'ok' })
    vi.mocked(api.operation).mockResolvedValueOnce({ operation_id: 'rollback-op', status: 'completed', data: { message: 'rolled back', status: 'Active' } })

    await fetchRollbackVersionPreview('3')
    await rollbackVersion('3')

    expect(api.get).toHaveBeenCalledWith('/api/v1/system/config/versions/3/rollback-preview')
    expect(api.operation).toHaveBeenCalledWith('POST', '/api/v1/system/config/versions/3/rollback', undefined, undefined, expect.any(String))
  })

  it('reuses the rollback idempotency key after a retryable operation error', async () => {
    const { api } = await import('./client')
    vi.mocked(api.operation)
      .mockResolvedValueOnce({ operation_id: 'rollback-op', status: 'failed', error: { code: 'failed', message: 'retry later' } })
      .mockResolvedValueOnce({ operation_id: 'rollback-op', status: 'completed', data: { message: 'rolled back', status: 'Active' } })

    await expect(rollbackVersion('retry-target')).rejects.toThrow('retry later')
    await rollbackVersion('retry-target')
    const calls = vi.mocked(api.operation).mock.calls.slice(-2)
    const firstKey = calls[0][4]
    const retryKey = calls[1][4]
    expect(retryKey).toBe(firstKey)
  })
})
