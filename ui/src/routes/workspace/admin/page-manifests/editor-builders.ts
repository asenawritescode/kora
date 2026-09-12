import type { DocType } from '@/types/kora'
import { PAGE_COMPONENT_LIBRARY, type PageComponent, type PageLayoutType, type PageManifest } from '../../../../manifest/schema/page'
import { bindComponentToPrimaryResource, selectListFields } from '../../../../manifest/runtime/standard-pages'

export type ComponentAvailability = { enabled: boolean; reason?: string }

export function addBoundComponent(manifest: PageManifest, componentType: string, doctype: DocType | null): PageManifest {
  const libraryEntry = PAGE_COMPONENT_LIBRARY.find((entry) => entry.component === componentType)
  const position = manifest.spec.layout.children.length
  const base: PageComponent = {
    id: `${componentType}_${Date.now()}`,
    component: componentType,
    version: 1,
    region: defaultRegionForLayout(manifest.spec.layout.type),
    position,
    span: manifest.spec.layout.type === 'grid' ? 6 : undefined,
    props: { title: libraryEntry?.label ?? componentType.replace(/_/g, ' ') },
    required_capabilities: [...(libraryEntry?.capabilities ?? [])],
    offline: manifest.spec.offline,
  }
  const component = doctype ? withDoctypeDefaults(base, doctype, position) : base

  return {
    ...manifest,
    spec: {
      ...manifest.spec,
      capabilities: Array.from(new Set([...manifest.spec.capabilities, ...(libraryEntry?.capabilities ?? [])])),
      layout: {
        ...manifest.spec.layout,
        children: [...manifest.spec.layout.children, component],
      },
    },
  }
}

/**
 * Move a top-level component within the semantic layout order. The editor is
 * allowed to rearrange components, but never writes editor-only coordinates;
 * `position` remains the sole persisted ordering contract.
 */
export function moveManifestComponent(manifest: PageManifest, componentID: string, targetIndex: number): PageManifest {
  const children = manifest.spec.layout.children
  const sourceIndex = children.findIndex((component) => component.id === componentID)
  if (sourceIndex < 0 || children.length < 2) return manifest

  const boundedIndex = Math.max(0, Math.min(targetIndex, children.length - 1))
  if (sourceIndex === boundedIndex) return manifest

  const nextChildren = [...children]
  const [moved] = nextChildren.splice(sourceIndex, 1)
  nextChildren.splice(boundedIndex, 0, moved)

  return {
    ...manifest,
    spec: {
      ...manifest.spec,
      layout: {
        ...manifest.spec.layout,
        children: nextChildren.map((component, index) => ({ ...component, position: index })),
      },
    },
  }
}

/** Return the generic schema prerequisites for a palette component. */
export function componentAvailability(componentType: string, doctype: DocType | null): ComponentAvailability {
  const needsRecord = ['record_table', 'record_list', 'record_cards', 'record_form', 'record_detail']
  if (needsRecord.includes(componentType) && !doctype) {
    return { enabled: false, reason: 'Choose a data source first.' }
  }
  if (!doctype) return { enabled: true }

  const fields = doctype.fields.filter((field) => !['Section Break', 'Column Break', 'Heading'].includes(field.fieldtype))
  if (componentType === 'chart' && !fields.some((field) => ['Int', 'Float', 'Currency', 'Percent'].includes(field.fieldtype))) {
    return { enabled: false, reason: 'Needs a numeric field.' }
  }
  if (componentType === 'calendar_view' && !fields.some((field) => ['Date', 'Datetime'].includes(field.fieldtype))) {
    return { enabled: false, reason: 'Needs a date or datetime field.' }
  }
  if (componentType === 'kanban_board' && !fields.some((field) => field.fieldtype === 'Select')) {
    return { enabled: false, reason: 'Needs a Select field for lanes.' }
  }
  return { enabled: true }
}

export function withDoctypeDefaults(component: PageComponent, doctype: DocType, position: number): PageComponent {
  const fields = selectListFields(doctype)
  const title = doctype.title_field || fields[0] || 'name'
  const bound = bindComponentToPrimaryResource(component, doctype, position)

  if (bound.component === 'record_table') {
    return {
      ...bound,
      props: {
        ...bound.props,
        desktop_columns: fields,
        mobile_columns: fields.slice(0, 3),
      },
    }
  }
  if (bound.component === 'record_cards') {
    return {
      ...bound,
      props: {
        ...bound.props,
        bindings: {
          title,
          subtitle: fields.find((field) => field !== title && field !== 'name') || title,
        },
      },
    }
  }
  return bound
}

export function getPrimaryDoctypeName(manifest: PageManifest): string {
  return String(manifest.spec.resources.find((resource) => resource.id === 'primary')?.params.doctype || '')
}

export function defaultRegionForLayout(layout: PageLayoutType): string {
  return layout === 'three_panel' ? 'main' : 'main'
}
