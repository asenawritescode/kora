import type { DocType, Field } from '@/types/kora'
import type { PageComponent, PageLayoutType, PageManifest, PageResource } from '../schema/page'
import { createBlankPageManifest } from '../schema/page'

export type StandardPageKind = 'table' | 'cards' | 'form' | 'detail' | 'overview'

export const STANDARD_PAGE_KINDS: Array<{ kind: StandardPageKind; label: string; description: string }> = [
  { kind: 'table', label: 'Standard table', description: 'Search and tabular records for operations teams.' },
  { kind: 'cards', label: 'Card board', description: 'Scannable customer-facing or mobile-friendly records.' },
  { kind: 'form', label: 'Create form', description: 'A clean entry screen for collecting new records.' },
  { kind: 'detail', label: 'Detail page', description: 'A focused record summary with supporting list context.' },
  { kind: 'overview', label: 'Custom overview', description: 'A guided landing page with search, filters, and a record table.' },
]

export function createStandardPageManifest(doctype: DocType, kind: StandardPageKind): PageManifest {
  const route = `/${slugify(doctype.name)}-${kind}`
  const listFields = selectListFields(doctype)
  const title = doctype.title_field || listFields[0] || 'name'
  const withInsights = kind !== 'form'
  const manifest = createBlankPageManifest()
  const baseCapabilities = capabilitiesForKind(kind)
  const capabilities = withInsights ? uniqueStrings([...baseCapabilities, 'analytics', 'charts']) : baseCapabilities
  const resources: PageResource[] = [
    {
      id: 'primary',
      query: 'document.list',
      params: { doctype: doctype.name, limit: 50 },
    },
  ]
  if (withInsights) {
    resources.push({
      id: 'insights',
      query: 'analytics.insights',
      params: { doctype: doctype.name },
    })
  }

  return {
    ...manifest,
    metadata: {
      ...manifest.metadata,
      name: `${slugify(doctype.name)}-${kind}`,
      package: doctype.module ? `tenant.${slugify(doctype.module)}` : manifest.metadata.package,
    },
    spec: {
      ...manifest.spec,
      route,
      capabilities,
      resources,
      layout: {
        type: layoutForKind(kind),
        columns: 12,
        children: componentsForKind(doctype, kind, listFields, title, withInsights),
      },
    },
  }
}

export function bindComponentToPrimaryResource(component: PageComponent, doctype: DocType | string, position: number): PageComponent {
  const doctypeName = typeof doctype === 'string' ? doctype : doctype.name
  return {
    ...component,
    position,
    data: component.component === 'search_box' || component.component === 'filter_bar' ? undefined : 'primary.data',
    props: {
      ...component.props,
      source_doctype: doctypeName,
    },
  }
}

export function selectListFields(doctype: DocType): string[] {
  const visible = doctype.fields
    .filter(isDisplayField)
    .filter((field) => field.in_list_view || field.fieldname === doctype.title_field)
    .map((field) => field.fieldname)
  const fallback = doctype.fields.filter(isDisplayField).map((field) => field.fieldname)
  return unique(['name', ...visible, ...(visible.length === 0 ? fallback : [])]).slice(0, 8)
}

function componentsForKind(doctype: DocType, kind: StandardPageKind, listFields: string[], title: string, withInsights: boolean): PageComponent[] {
  if (kind === 'form') {
    return [
      component('record_form', 'main', 0, {
        title: `New ${doctype.name}`,
        source_doctype: doctype.name,
      }, ['forms']),
    ]
  }

  if (kind === 'detail') {
    return [
      component('record_detail', 'main', 0, {
        title: `${doctype.name} detail`,
        source_doctype: doctype.name,
      }, ['detail']),
      component('record_table', 'side', 1, {
        title: `Recent ${doctype.name}`,
        source_doctype: doctype.name,
        desktop_columns: listFields.slice(0, 4),
        mobile_columns: listFields.slice(0, 3),
      }, ['tables']),
    ]
  }

  if (kind === 'overview') {
    return [
      component('search_box', 'main', 0, { title: `Search ${doctype.name}` }, ['filters'], undefined),
      component('filter_bar', 'main', 1, { title: `Filter ${doctype.name}` }, ['filters'], undefined),
      component('record_table', 'main', 2, {
        title: `${doctype.name} records`,
        source_doctype: doctype.name,
        desktop_columns: listFields,
        mobile_columns: listFields.slice(0, 3),
      }, ['tables']),
    ]
  }

  if (kind === 'cards') {
    return [
      insightsPanel(0, doctype, withInsights),
      component('search_box', 'main', 1, { title: `Search ${doctype.name}` }, ['filters'], undefined),
      component('filter_bar', 'main', 2, { title: `Filter ${doctype.name}` }, ['filters'], undefined),
      component('record_cards', 'main', 3, {
        title: doctype.name,
        source_doctype: doctype.name,
        bindings: {
          title,
          subtitle: listFields.find((field) => field !== title && field !== 'name') || title,
        },
      }, ['cards']),
    ]
  }

  return [
    component('search_box', 'main', 0, { title: `Search ${doctype.name}` }, ['filters'], undefined),
    component('filter_bar', 'main', 1, { title: `Filter ${doctype.name}` }, ['filters'], undefined),
    component('record_table', 'main', 2, {
      title: doctype.name,
      source_doctype: doctype.name,
      desktop_columns: listFields,
      mobile_columns: listFields.slice(0, 3),
    }, ['tables']),
  ]
}

function insightsPanel(position: number, doctype: DocType, enabled: boolean): PageComponent {
  return {
    id: 'insights_panel',
    component: 'insights_panel',
    version: 1,
    region: 'main',
    position,
    props: {
      title: `${doctype.name} insights`,
    },
    data: enabled ? 'insights.data' : 'primary.data',
    required_capabilities: enabled ? ['dashboard', 'charts', 'analytics'] : ['dashboard'],
    offline: 'read_only',
  }
}

function component(
  type: string,
  region: string,
  position: number,
  props: Record<string, unknown>,
  capabilities: string[],
  data = 'primary.data',
): PageComponent {
  return {
    id: `${type}_${position + 1}`,
    component: type,
    version: 1,
    region,
    position,
    props,
    data,
    required_capabilities: capabilities,
    offline: 'read_only',
  }
}

function capabilitiesForKind(kind: StandardPageKind): string[] {
  if (kind === 'form') return ['forms']
  if (kind === 'detail') return ['dashboard', 'detail', 'tables']
  if (kind === 'cards') return ['dashboard', 'cards', 'filters']
  if (kind === 'overview') return ['dashboard', 'tables', 'filters']
  return ['dashboard', 'tables', 'filters']
}

function uniqueStrings(values: string[]): string[] {
  return Array.from(new Set(values))
}

function layoutForKind(kind: StandardPageKind): PageLayoutType {
  return kind === 'detail' ? 'two_panel' : kind === 'overview' ? 'grid' : 'single'
}

function isDisplayField(field: Field): boolean {
  return !field.hidden && !['Section Break', 'Column Break', 'Heading', 'Table', 'Password', 'JSON'].includes(field.fieldtype)
}

function unique(values: string[]): string[] {
  return Array.from(new Set(values.filter(Boolean)))
}

function slugify(value: string): string {
  return value.trim().toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-|-$/g, '') || 'screen'
}
