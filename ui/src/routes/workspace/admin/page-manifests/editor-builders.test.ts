import { describe, expect, it } from 'vitest'
import type { DocType } from '../../../../types/kora'
import { createBlankPageManifest } from '../../../../manifest/schema/page'
import { addBoundComponent, getPrimaryDoctypeName, moveManifestComponent, withDoctypeDefaults } from './editor-builders'

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
  ],
}

describe('page manifest editor builders', () => {

  it('reorders semantic children and rewrites contiguous positions', () => {
    const manifest = createBlankPageManifest()
    const first = addBoundComponent(manifest, 'metric_card', null)
    const second = addBoundComponent(first, 'record_table', null)
    const third = addBoundComponent(second, 'record_form', null)

    const moved = moveManifestComponent(third, third.spec.layout.children[2].id, 0)

    expect(moved.spec.layout.children.map((component) => component.component)).toEqual([
      'record_form',
      'metric_card',
      'record_table',
    ])
    expect(moved.spec.layout.children.map((component) => component.position)).toEqual([0, 1, 2])
    expect(moveManifestComponent(moved, 'missing', 0)).toBe(moved)
  })
  it('binds record table components to the primary resource with responsive columns', () => {
    const base = {
      id: 'record_table_1',
      component: 'record_table',
      version: 1,
      region: 'main',
      position: 0,
      props: { title: 'Orders' },
      offline: 'read_only' as const,
    }

    expect(withDoctypeDefaults(base, doctype, 0)).toMatchObject({
      data: 'primary.data',
      props: {
        source_doctype: 'Sales Order',
        desktop_columns: ['name', 'customer_name', 'status', 'total'],
        mobile_columns: ['name', 'customer_name', 'status'],
      },
    })
  })

  it('binds record cards components to summary fields from the primary doctype', () => {
    const base = {
      id: 'record_cards_1',
      component: 'record_cards',
      version: 1,
      region: 'main',
      position: 0,
      props: { title: 'Orders' },
      offline: 'read_only' as const,
    }

    expect(withDoctypeDefaults(base, doctype, 0)).toMatchObject({
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

  it('adds bound components without losing existing layout semantics', () => {
    const manifest = createBlankPageManifest()
    const next = addBoundComponent(manifest, 'record_table', doctype)

    expect(next.spec.capabilities).toEqual(['tables'])
    expect(next.spec.layout.children).toHaveLength(1)
    expect(next.spec.layout.children[0]).toMatchObject({
      component: 'record_table',
      data: 'primary.data',
      props: { source_doctype: 'Sales Order' },
    })
  })

  it('reads the primary doctype name from the manifest resource binding', () => {
    const manifest = createBlankPageManifest()
    manifest.spec.resources = [
      { id: 'primary', query: 'document.list', params: { doctype: 'Sales Order' } },
    ]

    expect(getPrimaryDoctypeName(manifest)).toBe('Sales Order')
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
