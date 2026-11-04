import { describe, expect, it, vi } from 'vitest'
import { createBlankPageManifest } from '../schema/page'
import { createGeneratedRevision } from './generated-page'
import { loadGeneratedHistory, loadGeneratedRevision, saveGeneratedRevision } from './generated-page-store'

const tenant = { tenantId: 'store-test', theme: 'zinc' as const, allowedComponents: ['franken_card'] as const }
const fallback = { ...createBlankPageManifest(), metadata: { ...createBlankPageManifest().metadata, name: 'Fallback' } }

describe('generated page store', () => {
  it('writes current revision and history atomically', () => {
    const storage = new Map<string, string>()
    vi.stubGlobal('window', { localStorage: { getItem: (key: string) => storage.get(key) ?? null, setItem: (key: string, value: string) => storage.set(key, value) } })
    const revision = createGeneratedRevision({ ...createBlankPageManifest(), metadata: { ...createBlankPageManifest().metadata, name: 'Stored' } }, tenant, 3)
    saveGeneratedRevision(revision)
    expect(loadGeneratedRevision(tenant, fallback)).toEqual(revision)
    expect(loadGeneratedHistory(tenant.tenantId)).toEqual([revision])
    vi.unstubAllGlobals()
  })

  it('keeps the previous snapshot when localStorage rejects the next write', () => {
    const previous = createGeneratedRevision({ ...createBlankPageManifest(), metadata: { ...createBlankPageManifest().metadata, name: 'Previous' } }, tenant, 1)
    const storage = new Map([['kora.generated-page.v2.store-test', JSON.stringify({ revision: previous, history: [previous] })]])
    vi.stubGlobal('window', { localStorage: {
      getItem: (key: string) => storage.get(key) ?? null,
      setItem: () => { throw new DOMException('quota exceeded', 'QuotaExceededError') },
    } })
    expect(() => saveGeneratedRevision({ ...previous, revision: 2 })).toThrow('quota exceeded')
    expect(loadGeneratedRevision(tenant, fallback)).toEqual(previous)
    expect(loadGeneratedHistory(tenant.tenantId)).toEqual([previous])
    vi.unstubAllGlobals()
  })
})
