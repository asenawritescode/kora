import type { GeneratedPageRevision, GeneratedTenantConfig } from './generated-page'
import { acceptGeneratedPage, createGeneratedRevision } from './generated-page'
import type { PageManifest } from '../schema/page'

const STORAGE_PREFIX = 'kora.generated-page.v1'
const HISTORY_LIMIT = 20

export function loadGeneratedRevision(
  tenant: GeneratedTenantConfig,
  fallback: PageManifest,
): GeneratedPageRevision {
  if (typeof window === 'undefined') return createGeneratedRevision(fallback, tenant, 1)
  try {
    const raw = window.localStorage.getItem(storageKey(tenant.tenantId))
    if (!raw) return createGeneratedRevision(fallback, tenant, 1)
    const revision = JSON.parse(raw) as GeneratedPageRevision
    if (revision.tenantId !== tenant.tenantId || !revision.manifest) throw new Error('Invalid stored page.')
    if (acceptGeneratedPage(revision.manifest, tenant).issues.length) throw new Error('Stored page is no longer allowed.')
    return revision
  } catch {
    return createGeneratedRevision(fallback, tenant, 1)
  }
}

export function saveGeneratedRevision(revision: GeneratedPageRevision): void {
  if (typeof window === 'undefined') return
  window.localStorage.setItem(storageKey(revision.tenantId), JSON.stringify(revision))
  const history = loadGeneratedHistory(revision.tenantId)
  const next = [revision, ...history.filter((item) => item.revision !== revision.revision)].slice(0, HISTORY_LIMIT)
  window.localStorage.setItem(historyKey(revision.tenantId), JSON.stringify(next))
}

export function loadGeneratedHistory(tenantId: string): GeneratedPageRevision[] {
  if (typeof window === 'undefined') return []
  try {
    const value = JSON.parse(window.localStorage.getItem(historyKey(tenantId)) || '[]')
    return Array.isArray(value) ? value : []
  } catch { return [] }
}

function storageKey(tenantId: string): string {
  return `${STORAGE_PREFIX}.${tenantId}`
}

function historyKey(tenantId: string): string {
  return `${STORAGE_PREFIX}.${tenantId}.history`
}
