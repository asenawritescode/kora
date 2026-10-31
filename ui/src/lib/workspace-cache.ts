import type { QueryClient } from '@tanstack/react-query'

export function removeStaleWorkspaceQueries(client: QueryClient, tenantPrefix: string): void {
  client.removeQueries({
    predicate: (query) => !(query.queryKey[0] === 'navigation' && query.queryKey[1] === tenantPrefix),
  })
}
