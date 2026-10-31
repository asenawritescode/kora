import type { GeneratedPageRevision, GeneratedTenantConfig } from './generated-page'
import { acceptGeneratedPage, createGeneratedRevision } from './generated-page'
import type { PageManifest } from '../schema/page'

const STORAGE_PREFIX = 'kora.generated-page.v1'
const ATOMIC_STORAGE_PREFIX = 'kora.generated-page.v2'
const HISTORY_LIMIT = 20

interface StoredGeneratedPages {
  revision: GeneratedPageRevision
  history: GeneratedPageRevision[]
}

export function loadGeneratedRevision(
  tenant: GeneratedTenantConfig,
  fallback: PageManifest,
): GeneratedPageRevision {
  if (typeof window === 'undefined') return createGeneratedRevision(fallback, tenant, 1)
  try {
    const atomic = readAtomicStore(tenant.tenantId)
    const raw = atomic?.revision ?? window.localStorage.getItem(storageKey(tenant.tenantId))
    if (!raw) return createGeneratedRevision(fallback, tenant, 1)
    const revision = typeof raw === 'string' ? JSON.parse(raw) as GeneratedPageRevision : raw
    if (revision.tenantId !== tenant.tenantId || !revision.manifest) throw new Error('Invalid stored page.')
    if (acceptGeneratedPage(revision.manifest, tenant).issues.length) throw new Error('Stored page is no longer allowed.')
    return revision
  } catch {
    return createGeneratedRevision(fallback, tenant, 1)
  }
}

export function saveGeneratedRevision(revision: GeneratedPageRevision): void {
  if (typeof window === 'undefined') return
  const history = loadGeneratedHistory(revision.tenantId)
  const next = [revision, ...history.filter((item) => item.revision !== revision.revision)].slice(0, HISTORY_LIMIT)
  // One localStorage write is atomic. Keeping the current revision and its
  // history in separate keys allowed quota/security failures to persist only
  // half of a save.
  window.localStorage.setItem(atomicStorageKey(revision.tenantId), JSON.stringify({ revision, history: next } satisfies StoredGeneratedPages))
}

export function loadGeneratedHistory(tenantId: string): GeneratedPageRevision[] {
  if (typeof window === 'undefined') return []
  try {
    const atomic = readAtomicStore(tenantId)
    if (atomic) return atomic.history
    const value = JSON.parse(window.localStorage.getItem(historyKey(tenantId)) || '[]')
    return Array.isArray(value) ? value : []
  } catch { return [] }
}

function readAtomicStore(tenantId: string): StoredGeneratedPages | null {
  if (typeof window === 'undefined') return null
  const raw = window.localStorage.getItem(atomicStorageKey(tenantId))
  if (!raw) return null
  const value = JSON.parse(raw) as Partial<StoredGeneratedPages>
  if (!value.revision || value.revision.tenantId !== tenantId || !Array.isArray(value.history)) return null
  return { revision: value.revision, history: value.history }
}

function storageKey(tenantId: string): string {
  return `${STORAGE_PREFIX}.${tenantId}`
}

function historyKey(tenantId: string): string {
  return `${STORAGE_PREFIX}.${tenantId}.history`
}

function atomicStorageKey(tenantId: string): string {
  return `${ATOMIC_STORAGE_PREFIX}.${tenantId}`
}
