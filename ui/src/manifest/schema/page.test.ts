import { describe, expect, it } from 'vitest'
import { PAGE_COMPONENT_LIBRARY, createBlankPageManifest, normalizePageManifest, validatePageManifestContract, type PageManifest } from './page'

function validManifest(): PageManifest {
  return {
    ...createBlankPageManifest(),
    metadata: {
      name: 'sales-orders',
      version: '1.0.0',
      package: 'tenant.ops',
      status: 'draft',
    },
    spec: {
      ...createBlankPageManifest().spec,
      route: '/sales-orders',
      resources: [
        { id: 'orders', query: 'document.list', params: { doctype: 'Sales Order' } },
      ],
      actions: [
        { id: 'submit_order', command: 'document.submit', input: {}, invalidate: ['orders'] },
      ],
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
            props: { title: 'Orders' },
            data: 'orders.data',
            actions: ['submit_order'],
          },
        ],
      },
    },
  }
}

describe('PageManifest validation', () => {
  it('keeps the generic page palette limited to routed data-backed screen components', () => {
    const components = PAGE_COMPONENT_LIBRARY.map((entry) => entry.component)

    expect(components).toEqual([
      'dashboard_grid',
      'metric_card',
      'chart',
      'search_box',
      'filter_bar',
      'record_table',
      'record_list',
      'record_cards',
      'record_form',
      'record_detail',
      'approval_queue',
      'kanban_board',
      'calendar_view',
    ])
    expect(components).not.toEqual(expect.arrayContaining([
      'workspace_dashboard',
      'product_grid',
      'cart_panel',
      'payment_panel',
      'scanner_input',
      'wizard',
      'public_form',
    ]))
  })

  it('accepts a valid RFC page manifest', () => {
    expect(validatePageManifestContract(validManifest())).toEqual([])
  })

  it('reports exact manifest paths for invalid resource, action, and component references', () => {
    const manifest = validManifest()
    manifest.spec.resources = []
    manifest.spec.actions[0].invalidate = ['missing_resource']
    manifest.spec.layout.children[0].data = 'missing_resource.data'
    manifest.spec.layout.children[0].actions = ['missing_action']

    const issues = validatePageManifestContract(manifest)

    expect(issues).toEqual(expect.arrayContaining([
      expect.objectContaining({ path: 'spec.actions.submit_order.invalidate' }),
      expect.objectContaining({ path: 'spec.layout.children.0.data' }),
      expect.objectContaining({ path: 'spec.layout.children.0.actions' }),
    ]))
  })

  it('validates component resource role bindings against declared page resources', () => {
    const manifest = validManifest()
    manifest.spec.layout.children[0].resource_bindings = { categories: 'missing_categories' }

    expect(validatePageManifestContract(manifest)).toEqual(expect.arrayContaining([
      expect.objectContaining({ path: 'spec.layout.children.0.resource_bindings.categories' }),
    ]))
  })

  it('rejects malformed top-level manifest contract fields', () => {
    const manifest = validManifest()
    manifest.apiVersion = 'bad' as PageManifest['apiVersion']
    manifest.kind = 'Widget' as PageManifest['kind']
    manifest.spec.route = 'workspace/pages/orders'

    expect(validatePageManifestContract(manifest)).toEqual(expect.arrayContaining([
      { path: 'apiVersion', message: 'Use ui.kora.dev/v1.' },
      { path: 'kind', message: 'Use kind Page.' },
      { path: 'spec.route', message: 'Route must start with /.' },
    ]))
  })

  it('rejects reserved routes that would shadow Kigae core routes', () => {
    const manifest = validManifest()
    manifest.spec.route = '/workspace/admin/page-manifests'

    expect(validatePageManifestContract(manifest)).toEqual(expect.arrayContaining([
      { path: 'spec.route', message: 'Route conflicts with a reserved Kigae route.' },
    ]))
  })

  it('rejects unsupported resource queries and action commands', () => {
    const manifest = validManifest()
    manifest.spec.resources[0].query = 'sql.raw'
    manifest.spec.actions[0].command = 'javascript.eval'

    expect(validatePageManifestContract(manifest)).toEqual(expect.arrayContaining([
      { path: 'spec.resources.orders.query', message: 'Unsupported resource query sql.raw.' },
      { path: 'spec.actions.submit_order.command', message: 'Unsupported action command javascript.eval.' },
    ]))
  })

  it('rejects executable-looking binding paths', () => {
    const manifest = validManifest()
    manifest.spec.layout.children[0].data = 'orders.data;alert(1)'
    manifest.spec.layout.children[0].props = {
      bindings: {
        title: 'name, window.location',
        total: 'amount + tax',
      },
    }

    expect(validatePageManifestContract(manifest)).toEqual(expect.arrayContaining([
      { path: 'spec.layout.children.0.data', message: 'Data binding must be an allowlisted dotted path.' },
      { path: 'spec.layout.children.0.props.bindings.total', message: 'Binding values must be allowlisted dotted paths.' },
    ]))
  })

  it('rejects data components that are not bound to a resource', () => {
    const manifest = validManifest()
    manifest.spec.layout.children[0].data = undefined

    expect(validatePageManifestContract(manifest)).toEqual(expect.arrayContaining([
      { path: 'spec.layout.children.0.data', message: 'record_table needs a data resource binding.' },
    ]))
  })

  it('rejects component doctypes that disagree with the bound resource doctype', () => {
    const manifest = validManifest()
    manifest.spec.layout.children[0].props.source_doctype = 'Customer'

    expect(validatePageManifestContract(manifest)).toEqual(expect.arrayContaining([
      { path: 'spec.layout.children.0.props.source_doctype', message: 'Component source_doctype must match resource doctype Sales Order.' },
    ]))
  })

  it('normalizes manifest ordering before persistence', () => {
    const manifest = validManifest()
    manifest.spec.permissions = ['write', 'read', 'write']
    manifest.spec.capabilities = ['charts', 'dashboard', 'charts']
    manifest.spec.resources = [
      { id: 'z', query: 'document.list', params: { doctype: 'Sales Order' } },
      { id: 'a', query: 'document.list', params: { doctype: 'Customer' } },
    ]
    manifest.spec.actions = [
      { id: 'z_action', command: 'document.submit', input: {}, invalidate: ['z', 'a'] },
      { id: 'a_action', command: 'document.create', input: {}, invalidate: ['a'] },
    ]
    manifest.spec.layout.children = [
      { id: 'b', component: 'record_table', version: 1, region: 'main', position: 7, props: { title: 'B' }, data: 'z.data' },
      { id: 'a', component: 'record_list', version: 1, region: 'main', position: 3, props: { title: 'A' }, data: 'a.data' },
    ]

    const normalized = normalizePageManifest(manifest)
    expect(normalized.spec.capabilities).toEqual(['charts', 'dashboard'])
    expect(normalized.spec.permissions).toEqual(['read', 'write'])
    expect(normalized.spec.resources.map((resource) => resource.id)).toEqual(['a', 'z'])
    expect(normalized.spec.actions.map((action) => action.id)).toEqual(['a_action', 'z_action'])
    expect(normalized.spec.layout.children.map((component) => component.id)).toEqual(['a', 'b'])
    expect(normalized.spec.layout.children.map((component) => component.position)).toEqual([0, 1])
  })

  it('normalizes nested component trees deterministically for source round-trips', () => {
    const manifest = validManifest()
    manifest.spec.layout.children = [
      {
        id: 'parent',
        component: 'dashboard_grid',
        version: 1,
        region: 'main',
        position: 4,
        props: { title: 'Parent', desktop_columns: ['b', 'a', 'b'] },
        children: [
          {
            id: 'child-b',
            component: 'metric_card',
            version: 1,
            region: 'main',
            position: 2,
            props: { title: 'Child B' },
          },
          {
            id: 'child-a',
            component: 'metric_card',
            version: 1,
            region: 'main',
            position: 0,
            props: { title: 'Child A' },
          },
        ],
      },
    ]

    const normalized = normalizePageManifest(manifest)
    const roundTripped = JSON.parse(JSON.stringify(normalized)) as PageManifest

    expect(normalized.spec.layout.children[0].position).toBe(0)
    expect(normalized.spec.layout.children[0].children?.map((child) => child.id)).toEqual(['child-b', 'child-a'])
    expect(normalized.spec.layout.children[0].children?.map((child) => child.position)).toEqual([0, 1])
    expect(roundTripped).toEqual(normalized)
  })
})
