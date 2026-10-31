import { renderToStaticMarkup } from 'react-dom/server'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { describe, expect, it, vi } from 'vitest'
import type React from 'react'
import type { PageManifest } from '@/manifest/schema/page'
import { createSimulatedResourceState, ManifestRenderer } from './ManifestRenderer'
import { documentListParams, documentListResourceWithFilter } from './policy'

vi.mock('../../components/views/registry', () => ({
  resolveComponentEntry: (component: string) => {
    if (component === 'not_registered') return undefined
    return {
      component: ({ children, config, data }: { children?: React.ReactNode; config: { label?: string }; data?: unknown }) => (
        <div aria-label={config.label || 'Orders'}>{data ? JSON.stringify(data) : null}{children}</div>
      ),
    }
  },
  UnsupportedComponent: ({ config }: { config: { type?: string } }) => (
    <div>Unsupported component type: {config.type}</div>
  ),
}))

vi.mock('@/lib/api/analytics', () => ({
  fetchInsights: async () => ({ data: [] }),
}))

function manifest(overrides: Partial<PageManifest> = {}): PageManifest {
  const base: PageManifest = {
    apiVersion: 'ui.kora.dev/v1',
    kind: 'Page',
    metadata: {
      name: 'orders',
      version: '1.0.0',
      package: 'tenant.ops',
      status: 'draft',
    },
    spec: {
      route: '/orders',
      runtime: '>=2.0.0 <3.0.0',
      permissions: [],
      capabilities: ['tables'],
      offline: 'read_only',
      resources: [
        { id: 'orders', query: 'document.list', params: { doctype: 'Sales Order' } },
      ],
      actions: [],
      layout: {
        type: 'single',
        columns: 12,
        children: [
          {
            id: 'orders_table',
            component: 'record_table',
            version: 1,
            region: 'main',
            position: 0,
            props: { title: 'Orders', desktop_columns: ['name', 'status'] },
            data: 'orders.data',
            required_capabilities: ['tables'],
          },
        ],
      },
    },
  }

  return {
    ...base,
    ...overrides,
    metadata: { ...base.metadata, ...overrides.metadata },
    spec: { ...base.spec, ...overrides.spec },
  }
}

function renderManifest(
  page: PageManifest,
  mode: 'editor' | 'preview' | 'runtime' = 'preview',
  resourceKind: Parameters<typeof createSimulatedResourceState>[1] = 'normal',
) {
  const client = new QueryClient()
  return renderToStaticMarkup(
    <QueryClientProvider client={client}>
      <ManifestRenderer
        manifest={page}
        mode={mode}
        selectedComponentId="orders_table"
        resourceState={createSimulatedResourceState(page, resourceKind)}
      />
    </QueryClientProvider>,
  )
}

describe('ManifestRenderer', () => {
  it('sends bounded list criteria and visible-name search to the server query', () => {
    const resource = manifest().spec.resources[0]
    resource.params = {
      ...resource.params,
      limit: 25,
      filters: [{ field: 'status', op: '=', value: 'Open' }],
      order_by: 'modified DESC',
      fields: ['name', 'title'],
      search: 'Northwind',
    }
    expect(documentListParams(resource)).toEqual({
      limit: 25,
      filters: '[["status","=","Open"]]',
      order_by: 'modified DESC',
      fields: ['name', 'title'],
      search: 'Northwind',
    })
  })

  it('sends interactive search and filters as database list-query parameters', () => {
    const resource = manifest().spec.resources[0]
    resource.params = {
      ...resource.params,
      filters: [{ field: 'status', op: '=', value: 'Open' }],
    }

    const searched = documentListResourceWithFilter(resource, { field: '__search__', value: '  Northwind  ' })
    expect(documentListParams(searched)).toMatchObject({
      filters: '[["status","=","Open"]]',
      search: 'Northwind',
    })

    const filtered = documentListResourceWithFilter(resource, { field: 'category', value: 'Hardware' })
    expect(documentListParams(filtered).filters).toBe('[["status","=","Open"],["category","=","Hardware"]]')
  })

  it('retains configured status filters in the database list query', () => {
    const configured = manifest({
      spec: {
        ...manifest().spec,
        resources: [{ id: 'activities', query: 'document.list', params: {
          doctype: 'Activity', limit: 50,
          filters: [{ field: 'status', op: 'in', value: ['Open', 'In Progress'] }],
        } }],
      },
    })
    expect(documentListParams(configured.spec.resources[0]).filters)
      .toBe('[["status","in",["Open","In Progress"]]]')
  })

  it('resolves PageComponent types through the registered Kigae component registry', () => {
    const markup = renderManifest(manifest())

    expect(markup).toContain('data-manifest-render-mode="preview"')
    expect(markup).toContain('Sales Order-001')
    expect(markup).toContain('aria-label="Orders"')
  })

  it('renders unsupported components through a typed fallback', () => {
    const page = manifest({
      spec: {
        ...manifest().spec,
        layout: {
          ...manifest().spec.layout,
          children: [
            {
              id: 'unknown',
              component: 'not_registered',
              version: 1,
              region: 'main',
              position: 0,
              props: { title: 'Unknown' },
            },
          ],
        },
      },
    })

    expect(renderManifest(page)).toContain('Unsupported component type: not_registered')
  })

  it('wraps real components with selection chrome only in editor mode', () => {
    const markup = renderManifest(manifest(), 'editor')

    expect(markup).toContain('data-component-id="orders_table"')
    expect(markup).toContain('aria-pressed="true"')
    expect(markup).toContain('Duplicate')
    expect(markup).toContain('Remove')
    expect(markup).toContain('Sales Order-001')
  })

  it('renders preview mode without editor chrome', () => {
    const markup = renderManifest(manifest(), 'preview')

    expect(markup).toContain('data-component-id="orders_table"')
    expect(markup).not.toContain('aria-pressed')
    expect(markup).not.toContain('Duplicate')
    expect(markup).not.toContain('Remove')
  })

  it('renders permission-denied state as a closed gate', () => {
    const markup = renderManifest(manifest(), 'preview', 'permission_denied')

    expect(markup).toContain('You do not have access to this yet')
    expect(markup).toContain('You do not have access to this data yet.')
  })

  it('renders offline and conflict states with explicit copy', () => {
    expect(renderManifest(manifest(), 'preview', 'offline')).toContain('This data is unavailable while offline.')
    expect(renderManifest(manifest(), 'preview', 'conflict')).toContain('This data has a conflict that needs review.')
  })

  it('renders stale state as a refreshable read path', () => {
    const markup = renderManifest(manifest(), 'preview', 'stale')

    expect(markup).toContain('Refreshing')
    expect(markup).toContain('Showing stale data while the resource refreshes.')
  })

  it('valid source edits rerender through the same manifest runtime path', () => {
    const source = JSON.stringify({
      ...manifest(),
      spec: {
        ...manifest().spec,
        layout: {
          ...manifest().spec.layout,
          children: [
            {
              ...manifest().spec.layout.children[0],
              props: { title: 'Edited orders', desktop_columns: ['name'] },
            },
          ],
        },
      },
    })
    const parsed = JSON.parse(source) as PageManifest

    const markup = renderManifest(parsed)

    expect(markup).toContain('aria-label="Edited orders"')
    expect(markup).toContain('Sales Order-001')
  })
})
