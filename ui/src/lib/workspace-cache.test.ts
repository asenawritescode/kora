import { QueryClient } from '@tanstack/react-query'
import { afterEach, describe, expect, it } from 'vitest'
import { removeStaleWorkspaceQueries } from './workspace-cache'

describe('workspace query cleanup', () => {
  let client: QueryClient | undefined

  afterEach(() => {
    client?.clear()
    client = undefined
  })

  it('removes stale tenant projections and preserves active navigation', () => {
    client = new QueryClient()
    client.setQueryData(['navigation', '/s/old'], { tenant: 'old' })
    client.setQueryData(['navigation', '/s/new'], { tenant: 'new' })
    client.setQueryData(['requests', '/s/old', 'alice'], { items: ['private-old'] })
    client.setQueryData(['resource', 'Invoice'], { rows: ['private-old'] })

    removeStaleWorkspaceQueries(client, '/s/new')

    expect(client.getQueryData(['navigation', '/s/new'])).toEqual({ tenant: 'new' })
    expect(client.getQueryData(['navigation', '/s/old'])).toBeUndefined()
    expect(client.getQueryData(['requests', '/s/old', 'alice'])).toBeUndefined()
    expect(client.getQueryData(['resource', 'Invoice'])).toBeUndefined()
  })
})
