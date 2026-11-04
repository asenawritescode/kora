import { describe, expect, it } from 'vitest'
import type { DocType } from '@/types/kora'
import { validatePageManifestContract } from '../schema/page'
import { bindComponentToPrimaryResource, createStandardPageManifest, selectListFields } from './standard-pages'

const doctype: DocType = {
  name: 'Sales Order',
  module: 'Sales',
  is_submittable: true,
  is_child_table: false,
  is_single: false,
  track_changes: true,
  title_field: 'customer_name',
  search_fields: 'customer_name,status',
  sort_field: 'modified',
  sort_order: 'DESC',
  description: '',
  fields: [
    field('customer_name', 'Data', true),
    field('status', 'Select', true),
    field('total', 'Currency', true),
    field('internal_json', 'JSON', true),
    field('items', 'Table', true),
  ],
}

describe('standard page generator', () => {
  it('selects safe display fields from the doctype', () => {
    expect(selectListFields(doctype)).toEqual(['name', 'customer_name', 'status', 'total'])
  })

  it('generates a standard table page with a primary doctype resource', () => {
    const manifest = createStandardPageManifest(doctype, 'table')

    expect(manifest.spec.route).toBe('/sales-order-table')
    expect(manifest.spec.capabilities).toEqual(['dashboard', 'tables', 'filters', 'analytics', 'charts'])
    expect(manifest.spec.resources[0]).toMatchObject({
      id: 'primary',
      query: 'document.list',
      params: { doctype: 'Sales Order' },
    })
    expect(manifest.spec.layout.children.map((component) => component.component)).toEqual([
      'search_box',
      'filter_bar',
      'record_table',
    ])
    expect(manifest.spec.layout.children[0]).toMatchObject({
      component: 'search_box',
    })
    expect(manifest.spec.layout.children[1]).toMatchObject({
      component: 'filter_bar',
    })
    expect(manifest.spec.layout.children[2]).toMatchObject({
      component: 'record_table',
      data: 'primary.data',
      props: {
        source_doctype: 'Sales Order',
        desktop_columns: ['name', 'customer_name', 'status', 'total'],
      },
    })
  })

  it('binds guided card pages to the primary resource and summary fields', () => {
    const manifest = createStandardPageManifest(doctype, 'cards')

    expect(manifest.spec.layout.type).toBe('single')
    expect(manifest.spec.layout.children.map((component) => component.component)).toEqual([
      'insights_panel',
      'search_box',
      'filter_bar',
      'record_cards',
    ])
    expect(manifest.spec.layout.children[1]).toMatchObject({
      component: 'search_box',
      data: 'primary.data',
    })
    expect(manifest.spec.layout.children[2]).toMatchObject({
      component: 'filter_bar',
      data: 'primary.data',
    })
    expect(manifest.spec.layout.children[3]).toMatchObject({
      data: 'primary.data',
      props: {
        source_doctype: 'Sales Order',
        bindings: {
          title: 'customer_name',
          subtitle: 'status',
        },
      },
    })
  })

  it('generates contract-valid guided templates for every supported preset', () => {
    for (const kind of ['table', 'cards', 'form', 'detail', 'overview'] as const) {
      const manifest = createStandardPageManifest(doctype, kind)

      expect(validatePageManifestContract(manifest)).toEqual([])
      expect(manifest.metadata.name).toBe(`sales-order-${kind}`)
      expect(manifest.spec.route).toBe(`/${'sales-order'}-${kind}`)
      expect(manifest.spec.layout.children.length).toBeGreaterThan(0)
      expect(manifest.spec.layout.children.every((component) => component.region === 'main' || component.region === 'side')).toBe(true)
      const bound = manifest.spec.layout.children.find((component) => component.props.source_doctype === 'Sales Order')
      expect(bound).toBeDefined()
    }
  })

  it('binds added components to the primary resource and doctype', () => {
    const component = bindComponentToPrimaryResource({
      id: 'cards',
      component: 'record_cards',
      version: 1,
      region: 'main',
      position: 0,
      props: {},
    }, doctype, 2)

    expect(component).toMatchObject({
      data: 'primary.data',
      position: 2,
      props: { source_doctype: 'Sales Order' },
    })
  })

  it('adds charts capability for overview pages so insights panels resolve', () => {
    const manifest = createStandardPageManifest(doctype, 'overview')

    expect(manifest.spec.capabilities).toContain('charts')
    expect(manifest.spec.layout.children.map((component) => component.component)).toEqual([
      'search_box',
      'filter_bar',
      'record_table',
    ])
  })

  it('keeps generated form and detail pages contract-valid', () => {
    expect(validatePageManifestContract(createStandardPageManifest(doctype, 'form'))).toEqual([])
    expect(validatePageManifestContract(createStandardPageManifest(doctype, 'detail'))).toEqual([])
  })
})

function field(fieldname: string, fieldtype: DocType['fields'][number]['fieldtype'], inList: boolean): DocType['fields'][number] {
  return {
    fieldname,
    fieldtype,
    label: fieldname,
    options: '',
    reqd: false,
    unique: false,
    default: '',
    hidden: false,
    read_only: false,
    bold: false,
    in_list_view: inList,
    in_standard_filter: false,
    search_index: false,
    description: '',
    depends_on: '',
    mandatory_depends_on: '',
    constraints: null,
    renamed_from: '',
  }
}
