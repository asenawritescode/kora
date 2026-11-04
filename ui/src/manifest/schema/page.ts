import {
  actionPolicyError,
  componentNeedsResource,
  isReservedManifestRoute,
  isSafeBindingPath,
  isSafeManifestRoute,
  resourcePolicyError,
} from '../runtime/policy'

export type PageLifecycleStatus = 'draft' | 'preview' | 'active' | 'retired'
export type OfflinePolicy = 'unsupported' | 'read_only' | 'queue_writes' | 'full_slice'
export type PageLayoutType = 'single' | 'two_panel' | 'three_panel' | 'grid'

export interface PageManifest {
  apiVersion: 'ui.kora.dev/v1'
  kind: 'Page'
  metadata: {
    name: string
    version: string
    package: string
    status: PageLifecycleStatus
    hash?: string
  }
  spec: {
    route: string
    runtime: string
    permissions: string[]
    capabilities: string[]
    offline: OfflinePolicy
    resources: PageResource[]
    actions: PageAction[]
    layout: PageLayout
  }
}

export interface PageResource {
  id: string
  query: string
  params: Record<string, unknown>
  depends_on?: string[]
}

export interface PageAction {
  id: string
  command: string
  input: Record<string, unknown>
  invalidate: string[]
  offline?: Extract<OfflinePolicy, 'unsupported' | 'queue_writes'>
  confirmation?: boolean
}

export interface PageLayout {
  type: PageLayoutType
  columns?: number
  children: PageComponent[]
}

export interface PageComponent {
  id: string
  component: string
  version: number
  region: string
  position: number
  span?: number
  props: Record<string, unknown>
  data?: string
  resource_bindings?: Record<string, string>
  actions?: string[]
  required_capabilities?: string[]
  permissions?: string[]
  offline?: OfflinePolicy
  children?: PageComponent[]
}

export interface PageManifestValidationIssue {
  path: string
  message: string
}

export const PAGE_COMPONENT_LIBRARY = [
  { component: 'dashboard_grid', label: 'Dashboard grid', group: 'layout', capabilities: ['dashboard'] },
  { component: 'metric_card', label: 'Metric card', group: 'data', capabilities: ['dashboard'] },
  { component: 'chart', label: 'Chart', group: 'data', capabilities: ['charts'] },
  { component: 'search_box', label: 'Search box', group: 'data', capabilities: ['filters'] },
  { component: 'filter_bar', label: 'Filter bar', group: 'data', capabilities: ['filters'] },
  { component: 'record_table', label: 'Record table', group: 'data', capabilities: ['tables'] },
  { component: 'record_list', label: 'Record list', group: 'data', capabilities: ['lists'] },
  { component: 'record_cards', label: 'Record cards', group: 'data', capabilities: ['cards'] },
  { component: 'record_form', label: 'Record form', group: 'forms', capabilities: ['forms'] },
  { component: 'record_detail', label: 'Record detail', group: 'forms', capabilities: ['detail'] },
  { component: 'approval_queue', label: 'Approval queue', group: 'workflow', capabilities: ['workflow'] },
  { component: 'kanban_board', label: 'Kanban board', group: 'operations', capabilities: ['kanban'] },
  { component: 'calendar_view', label: 'Calendar', group: 'operations', capabilities: ['calendar'] },
] as const

export function createBlankPageManifest(): PageManifest {
  return {
    apiVersion: 'ui.kora.dev/v1',
    kind: 'Page',
    metadata: {
      name: '',
      version: '0.1.0',
      package: 'tenant.workspace',
      status: 'draft',
    },
    spec: {
      route: '/new-screen',
      runtime: '>=2.0.0 <3.0.0',
      permissions: [],
      capabilities: [],
      offline: 'unsupported',
      resources: [],
      actions: [],
      layout: {
        type: 'single',
        columns: 12,
        children: [],
      },
    },
  }
}

export function validatePageManifestContract(manifest: PageManifest): PageManifestValidationIssue[] {
  const issues: PageManifestValidationIssue[] = []
  const componentIds = new Set<string>()
  const resourceIds = new Set<string>()
  const resourcesById = new Map<string, PageResource>()
  const actionIds = new Set<string>()

  if (manifest.apiVersion !== 'ui.kora.dev/v1') issues.push({ path: 'apiVersion', message: 'Use ui.kora.dev/v1.' })
  if (manifest.kind !== 'Page') issues.push({ path: 'kind', message: 'Use kind Page.' })
  if (!manifest.metadata.name.trim()) issues.push({ path: 'metadata.name', message: 'Name is required before saving.' })
  if (!manifest.metadata.version.trim()) issues.push({ path: 'metadata.version', message: 'Version is required.' })
  if (!manifest.metadata.package.trim()) issues.push({ path: 'metadata.package', message: 'Package is required.' })
  if (!manifest.spec.route.startsWith('/')) issues.push({ path: 'spec.route', message: 'Route must start with /.' })
  if (manifest.spec.route.startsWith('/') && !isSafeManifestRoute(manifest.spec.route)) {
    issues.push({ path: 'spec.route', message: 'Route must be a single safe page segment like /pos.' })
  }
  if (isReservedManifestRoute(manifest.spec.route)) {
    issues.push({ path: 'spec.route', message: 'Route conflicts with a reserved Kigae route.' })
  }
  if (!manifest.spec.runtime.trim()) issues.push({ path: 'spec.runtime', message: 'Runtime range is required.' })
  if (!['single', 'two_panel', 'three_panel', 'grid'].includes(manifest.spec.layout.type)) {
    issues.push({ path: 'spec.layout.type', message: 'Choose a supported layout type.' })
  }

  for (const resource of manifest.spec.resources) {
    if (!resource.id.trim()) issues.push({ path: 'spec.resources', message: 'Every resource needs an id.' })
    if (resourceIds.has(resource.id)) issues.push({ path: `spec.resources.${resource.id}`, message: 'Resource ids must be unique.' })
    resourceIds.add(resource.id)
    resourcesById.set(resource.id, resource)
    if (!resource.query.trim()) issues.push({ path: `spec.resources.${resource.id}.query`, message: 'Resource query is required.' })
    const policyError = resourcePolicyError(resource)
    if (policyError) issues.push({ path: `spec.resources.${resource.id}.query`, message: policyError })
  }

  for (const action of manifest.spec.actions) {
    if (!action.id.trim()) issues.push({ path: 'spec.actions', message: 'Every action needs an id.' })
    if (actionIds.has(action.id)) issues.push({ path: `spec.actions.${action.id}`, message: 'Action ids must be unique.' })
    actionIds.add(action.id)
    if (!action.command.trim()) issues.push({ path: `spec.actions.${action.id}.command`, message: 'Action command is required.' })
    const policyError = actionPolicyError(action)
    if (policyError) issues.push({ path: `spec.actions.${action.id}.command`, message: policyError })
    for (const tag of action.invalidate) {
      if (!resourceIds.has(tag)) issues.push({ path: `spec.actions.${action.id}.invalidate`, message: `Unknown invalidation resource ${tag}.` })
    }
  }

  visitComponents(manifest.spec.layout.children, (component, path) => {
    if (!component.id.trim()) issues.push({ path, message: 'Every component needs an id.' })
    if (componentIds.has(component.id)) issues.push({ path, message: `Duplicate component id ${component.id}.` })
    componentIds.add(component.id)
    if (!component.component.trim()) issues.push({ path, message: 'Component type is required.' })
    if (component.version < 1) issues.push({ path: `${path}.version`, message: 'Component version must be 1 or higher.' })
    if (componentNeedsResource(component.component) && !component.data) {
      issues.push({ path: `${path}.data`, message: `${component.component} needs a data resource binding.` })
    }
    if (component.data) {
      if (!isSafeBindingPath(component.data)) {
        issues.push({ path: `${path}.data`, message: 'Data binding must be an allowlisted dotted path.' })
      } else if (!resourceIds.has(component.data.split('.')[0])) {
        issues.push({ path: `${path}.data`, message: `Unknown data resource ${component.data}.` })
      } else {
        const resource = resourcesById.get(component.data.split('.')[0])
        const sourceDoctype = component.props.source_doctype
        const resourceDoctype = resource?.params.doctype
        if (typeof sourceDoctype === 'string' && typeof resourceDoctype === 'string' && sourceDoctype !== resourceDoctype) {
          issues.push({ path: `${path}.props.source_doctype`, message: `Component source_doctype must match resource doctype ${resourceDoctype}.` })
        }
      }
    }
    for (const [role, resourceID] of Object.entries(component.resource_bindings ?? {})) {
      if (!resourceID.trim()) issues.push({ path: `${path}.resource_bindings.${role}`, message: 'Resource role bindings must name a page resource.' })
      else if (!resourceIds.has(resourceID)) issues.push({ path: `${path}.resource_bindings.${role}`, message: `Unknown resource ${resourceID} for role ${role}.` })
    }
    const bindings = component.props.bindings
    if (bindings && typeof bindings === 'object' && !Array.isArray(bindings)) {
      for (const [key, value] of Object.entries(bindings as Record<string, unknown>)) {
        if (typeof value !== 'string') continue
        const paths = value.split(',').map((entry) => entry.trim()).filter(Boolean)
        if (paths.some((entry) => !isSafeBindingPath(entry))) {
          issues.push({ path: `${path}.props.bindings.${key}`, message: 'Binding values must be allowlisted dotted paths.' })
        }
      }
    }
    for (const action of component.actions ?? []) {
      if (!actionIds.has(action)) issues.push({ path: `${path}.actions`, message: `Unknown action ${action}.` })
    }
  })

  return issues
}

export function addManifestComponent(manifest: PageManifest, componentType: string): PageManifest {
  const libraryEntry = PAGE_COMPONENT_LIBRARY.find((entry) => entry.component === componentType)
  const component: PageComponent = {
    id: `${componentType}_${Date.now()}`,
    component: componentType,
    version: 1,
    region: defaultRegionForLayout(manifest.spec.layout.type),
    position: manifest.spec.layout.children.length,
    span: manifest.spec.layout.type === 'grid' ? 6 : undefined,
    props: { title: libraryEntry?.label ?? componentType.replace(/_/g, ' ') },
    required_capabilities: [...(libraryEntry?.capabilities ?? [])],
    offline: manifest.spec.offline,
  }

  return normalizePageManifest({
    ...manifest,
    spec: {
      ...manifest.spec,
      capabilities: Array.from(new Set([...manifest.spec.capabilities, ...(libraryEntry?.capabilities ?? [])])),
      layout: {
        ...manifest.spec.layout,
        children: [...manifest.spec.layout.children, component],
      },
    },
  })
}

export function removeManifestComponent(manifest: PageManifest, id: string): PageManifest {
  return normalizePageManifest({
    ...manifest,
    spec: {
      ...manifest.spec,
      layout: {
        ...manifest.spec.layout,
        children: manifest.spec.layout.children.filter((component) => component.id !== id),
      },
    },
  })
}

export function normalizePageManifest(manifest: PageManifest): PageManifest {
  const layout = manifest.spec.layout
  const normalizedChildren = [...layout.children]
    .sort((a, b) => a.position - b.position || a.id.localeCompare(b.id))
    .map((component, index) => normalizePageComponent(component, index))

  return {
    ...manifest,
    spec: {
      ...manifest.spec,
      capabilities: Array.from(new Set(manifest.spec.capabilities)).sort(),
      permissions: Array.from(new Set(manifest.spec.permissions)).sort(),
      resources: [...manifest.spec.resources].sort((a, b) => a.id.localeCompare(b.id)),
      actions: [...manifest.spec.actions].sort((a, b) => a.id.localeCompare(b.id)),
      layout: {
        ...layout,
        children: normalizedChildren,
      },
    },
  }
}

function normalizePageComponent(component: PageComponent, position: number): PageComponent {
  return {
    ...component,
    position,
    required_capabilities: component.required_capabilities ? [...new Set(component.required_capabilities)].sort() : undefined,
    permissions: component.permissions ? [...new Set(component.permissions)].sort() : undefined,
    actions: component.actions ? [...new Set(component.actions)].sort() : undefined,
    children: component.children ? [...component.children].map((child, index) => normalizePageComponent(child, index)) : undefined,
  }
}

function visitComponents(components: PageComponent[], visit: (component: PageComponent, path: string) => void, prefix = 'spec.layout.children') {
  components.forEach((component, index) => {
    const path = `${prefix}.${index}`
    visit(component, path)
    if (component.children?.length) visitComponents(component.children, visit, `${path}.children`)
  })
}

function defaultRegionForLayout(layout: PageLayoutType): string {
  return layout === 'three_panel' ? 'main' : 'main'
}
