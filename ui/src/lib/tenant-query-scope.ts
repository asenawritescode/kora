import { hashKey, type QueryKey } from '@tanstack/react-query'
import { getSitePrefix } from './basepath'

/**
 * Return a browser-origin and tenant-path scope for the query cache.
 * Host-based tenants are separated by origin; path-based tenants are separated
 * by their /s/:site prefix. The current router selects that prefix at page load.
 */
export function tenantQueryScope(): string {
  if (typeof window === 'undefined') return 'server'
  return `${window.location.origin}${getSitePrefix()}`
}

/** Keep otherwise-identical workspace query keys isolated between tenants. */
export function hashTenantQueryKey(queryKey: QueryKey): string {
  return hashKey([tenantQueryScope(), queryKey])
}
