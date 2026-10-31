import { afterEach, describe, expect, it, vi } from 'vitest'
import { QueryClient, QueryObserver } from '@tanstack/react-query'
import { hashTenantQueryKey, tenantQueryScope } from './tenant-query-scope'
import { removeStaleWorkspaceQueries } from './workspace-cache'

describe('tenant query cache scope', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('separates the same query key across path-routed tenants', () => {
    vi.stubGlobal('window', { location: { origin: 'https://kora.test', pathname: '/s/tenant-a/workspace' } })
    const tenantAHash = hashTenantQueryKey(['admin', 'users'])

    vi.stubGlobal('window', { location: { origin: 'https://kora.test', pathname: '/s/tenant-b/workspace' } })
    const tenantBHash = hashTenantQueryKey(['admin', 'users'])

    expect(tenantQueryScope()).toBe('https://kora.test/s/tenant-b')
    expect(tenantBHash).not.toBe(tenantAHash)
  })

  it('does not return a prior tenant projection after a same-tab tenant switch', () => {
    vi.stubGlobal('window', { location: { origin: 'https://kora.test', pathname: '/s/tenant-a/workspace' } })
    const client = new QueryClient({ defaultOptions: { queries: { queryKeyHashFn: hashTenantQueryKey, retry: false } } })
    client.setQueryData(['admin', 'users'], [{ name: 'tenant-a-user' }])
    const observer = new QueryObserver(client, {
      queryKey: ['admin', 'users'],
      queryFn: async () => [{ name: 'tenant-b-user' }],
      enabled: false,
    })
    const unsubscribe = observer.subscribe(() => undefined)
    expect(observer.getCurrentResult().data).toEqual([{ name: 'tenant-a-user' }])

    vi.stubGlobal('window', { location: { origin: 'https://kora.test', pathname: '/s/tenant-b/workspace' } })
    observer.setOptions({
      queryKey: ['admin', 'users'],
      queryFn: async () => [{ name: 'tenant-b-user' }],
      enabled: false,
    })
    expect(observer.getCurrentResult().data).toBeUndefined()
    removeStaleWorkspaceQueries(client, '/s/tenant-b')
    expect(client.getQueryData(['admin', 'users'])).toBeUndefined()

    client.setQueryData(['admin', 'users'], [{ name: 'tenant-b-user' }])
    expect(client.getQueryData(['admin', 'users'])).toEqual([{ name: 'tenant-b-user' }])
    unsubscribe()
    client.clear()
  })
})
