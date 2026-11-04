import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { PageManifest } from '@/manifest/schema/page'
import {
  buildActionBindingOptions,
  buildPublishPreflight,
  buildResourceBindingOptions,
  clearManifestDraft,
  manifestDraftStorageKey,
  previewViewportClass,
  previewViewportOptions,
  readManifestDraft,
  writeManifestDraft,
} from './editor-helpers'

function manifest(): PageManifest {
  return {
    apiVersion: 'ui.kora.dev/v1',
    kind: 'Page',
    metadata: {
      name: 'sales-order-table',
      version: '1.0.0',
      package: 'tenant.sales',
      status: 'draft',
    },
    spec: {
      route: '/sales-order-table',
      runtime: '>=2.0.0 <3.0.0',
      permissions: [],
      capabilities: [],
      offline: 'unsupported',
      resources: [
        { id: 'primary', query: 'document.list', params: { doctype: 'Sales Order' } },
        { id: 'insights', query: 'analytics.insights', params: { doctype: 'Sales Order' } },
      ],
      actions: [
        { id: 'refresh', command: 'document.update', input: {}, invalidate: ['primary'] },
      ],
      layout: {
        type: 'single',
        columns: 12,
        children: [
          {
            id: 'table',
            component: 'record_table',
            version: 1,
            region: 'main',
            position: 0,
            props: { title: 'Orders', source_doctype: 'Sales Order' },
            data: 'primary.data',
          },
        ],
      },
    },
  }
}

beforeEach(() => {
  vi.stubGlobal('window', globalThis.window ?? {})
  const store = new Map<string, string>()
  vi.stubGlobal('localStorage', {
    getItem: (key: string) => store.get(key) ?? null,
    setItem: (key: string, value: string) => {
      store.set(key, value)
    },
    removeItem: (key: string) => {
      store.delete(key)
    },
    clear: () => store.clear(),
  })
  localStorage.clear()
})

describe('page manifest editor helpers', () => {
  it('offers resource binding options for the actual manifest resources', () => {
    expect(buildResourceBindingOptions(manifest())).toEqual([
      { value: 'primary.data', label: 'primary', description: 'document.list for Sales Order' },
      { value: 'insights.data', label: 'insights', description: 'analytics.insights for Sales Order' },
    ])
  })

  it('offers action binding options for the actual manifest actions', () => {
    expect(buildActionBindingOptions(manifest())).toEqual([
      { value: 'refresh', label: 'refresh', description: 'document.update → primary' },
    ])
  })

  it('builds a publish preflight from manifest validation and unsupported surfaces', () => {
    const preflight = buildPublishPreflight(manifest())

    expect(preflight.canPublish).toBe(true)
    expect(preflight.issues).toEqual([])
    expect(preflight.resourceCount).toBe(2)
    expect(preflight.actionCount).toBe(1)
    expect(preflight.unsupportedResources).toEqual([])
    expect(preflight.unsupportedActions).toEqual([])
  })

  it('fails closed on unsupported queries and commands', () => {
    const next = manifest()
    next.spec.resources[0].query = 'sql.raw' as unknown as typeof next.spec.resources[0]['query']
    next.spec.actions[0].command = 'javascript.eval' as unknown as typeof next.spec.actions[0]['command']

    const preflight = buildPublishPreflight(next)

    expect(preflight.canPublish).toBe(false)
    expect(preflight.unsupportedResources).toEqual(['primary'])
    expect(preflight.unsupportedActions).toEqual(['refresh'])
  })

  it('round-trips local drafts through scoped storage', () => {
    const snapshot = {
      manifest: manifest(),
      savedAt: '2026-08-27T00:00:00.000Z',
      source: 'editor' as const,
    }

    writeManifestDraft('sales-order-table', snapshot)

    expect(manifestDraftStorageKey('sales-order-table')).toBe('kora_page_manifest_draft:sales-order-table')
    expect(readManifestDraft('sales-order-table')).toEqual(snapshot)

    clearManifestDraft('sales-order-table')
    expect(readManifestDraft('sales-order-table')).toBeNull()
  })

  it('surfaces failed draft writes and clears', () => {
    const snapshot = { manifest: manifest(), savedAt: '2026-08-27T00:00:00.000Z', source: 'editor' as const }
    vi.stubGlobal('localStorage', {
      getItem: () => null,
      setItem: () => { throw new DOMException('quota exceeded', 'QuotaExceededError') },
      removeItem: () => { throw new DOMException('storage disabled', 'SecurityError') },
      clear: () => undefined,
    })

    expect(() => writeManifestDraft('sales-order-table', snapshot)).toThrow('quota exceeded')
    expect(() => clearManifestDraft('sales-order-table')).toThrow('storage disabled')
  })

  it('surfaces local draft read access failures', () => {
    vi.stubGlobal('localStorage', {
      getItem: () => { throw new DOMException('storage disabled', 'SecurityError') },
      setItem: () => undefined,
      removeItem: () => undefined,
      clear: () => undefined,
    })

    expect(() => readManifestDraft('sales-order-table')).toThrow('storage disabled')
  })

  it('exposes deterministic preview viewport presets', () => {
    expect(previewViewportOptions()).toEqual([
      { value: 'desktop', label: 'Desktop', description: 'Full-width preview' },
      { value: 'tablet', label: 'Tablet', description: 'Mid-width preview' },
      { value: 'mobile', label: 'Mobile', description: 'Narrow preview' },
    ])
    expect(previewViewportClass('desktop')).toBe('w-full')
    expect(previewViewportClass('tablet')).toBe('mx-auto w-full max-w-4xl')
    expect(previewViewportClass('mobile')).toBe('mx-auto w-full max-w-sm')
  })
})
