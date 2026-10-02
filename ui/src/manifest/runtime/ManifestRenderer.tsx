import { Suspense, useMemo, useState } from 'react'
import { useQueries, keepPreviousData } from '@tanstack/react-query'
import { AlertTriangle, Ban, CloudOff, Loader2, RefreshCcw, ShieldAlert, WifiOff } from 'lucide-react'
import type { PageComponent, PageManifest, PageResource } from '@/manifest/schema/page'
import { resolveComponentEntry, UnsupportedComponent, type RegisteredComponentConfig } from '../../components/views/registry'
import { Badge } from '../../components/ui/badge'
import { Button } from '../../components/ui/button'
import { cn } from '../../lib/utils'
import { fetchInsights } from '@/lib/api/analytics'
import { isAllowedResourceQuery, resourcePolicyError } from './policy'
import { useCartStore } from '@/lib/cart-store'

export type ManifestRenderMode = 'editor' | 'preview' | 'runtime'
export type ResourceSimulationKind = 'normal' | 'loading' | 'empty' | 'error' | 'permission_denied' | 'offline' | 'conflict' | 'stale'

export interface ManifestResourceState {
  kind: ResourceSimulationKind
  data?: any
  message?: string
}

export interface ManifestRendererProps {
  manifest: PageManifest
  mode: ManifestRenderMode
  resourceState?: Record<string, ManifestResourceState>
  selectedComponentId?: string | null
  onSelectComponent?: (id: string | null) => void
  onDuplicateComponent?: (component: PageComponent) => void
  onRemoveComponent?: (id: string) => void
  onAction?: (actionId: string, context: Record<string, unknown>) => unknown | Promise<unknown>
  className?: string
}

export function ManifestRenderer({
  manifest,
  mode,
  resourceState,
  selectedComponentId,
  onSelectComponent,
  onDuplicateComponent,
  onRemoveComponent,
  onAction,
  className,
}: ManifestRendererProps) {
  const runtimeResources = useManifestRuntimeResources(manifest, !resourceState)
  const resources = resourceState ?? runtimeResources
  const [filters, setFilters] = useState<Record<string, { field?: string; value?: string }>>({})
  const filteredResources = useMemo(() => applyResourceFilters(manifest, resources, filters), [manifest, resources, filters])
  const regions = regionsForLayout(manifest.spec.layout.type)
  const addCartItem = useCartStore((state) => state.addItem)

  return (
    <div className={cn('space-y-4', className)} data-manifest-render-mode={mode}>
      <div className={layoutClass(manifest.spec.layout.type)}>
        {regions.map((region) => {
          const components = manifest.spec.layout.children
            .filter((component) => (component.region || 'main') === region.id)
            .sort((a, b) => a.position - b.position)

          return (
            <section
              key={region.id}
              className={regionClass(manifest.spec.layout.type, region.id)}
              aria-label={region.label}
              data-manifest-region={region.id}
            >
              {mode === 'editor' && (
                <div className="mb-2 flex items-center gap-2 text-[11px] font-semibold uppercase tracking-wide text-muted-foreground">
                  <span className="h-2 w-2 rounded-full bg-primary/50" />
                  {region.label}
                </div>
              )}
              {components.length === 0 && mode === 'editor' ? (
                <div className="flex min-h-40 items-center justify-center rounded-xl border border-dashed bg-muted/20 p-6 text-center text-sm text-muted-foreground">
                  Add a block to this region from the palette.
                </div>
              ) : components.map((component) => (
                <ManifestComponentRenderer
                  key={component.id}
                  component={component}
                  manifest={manifest}
                  mode={mode}
                  resources={filteredResources}
                  selected={selectedComponentId === component.id}
                  onSelectComponent={onSelectComponent}
                  onDuplicateComponent={onDuplicateComponent}
                  onRemoveComponent={onRemoveComponent}
                  onAction={(actionId, context) => {
                    const action = manifest.spec.actions.find((item) => item.id === actionId)
                    if (action?.command === 'local_cart_add') {
                      addCartItem(context)
                      return
                    }
                    if (actionId === 'filter' || actionId === 'search') {
                      const resourceId = String(component.data || manifest.spec.resources[0]?.id || '').split('.')[0]
                      if (resourceId) {
                        const category = context.category
                        const componentBindings = component.props.bindings as Record<string, unknown> | undefined
                        const field = category !== undefined
                          ? String(componentBindings?.group_field || 'category')
                          : String(context.field || '')
                        const value = category !== undefined ? String(category || '') : String(context.value || '')
                        setFilters((current) => ({
                          ...current,
                          [resourceId]: actionId === 'filter'
                            ? { field, value }
                            : { field: '__search__', value: String(context.value || '') },
                        }))
                      }
                    }
                    return onAction?.(actionId, { ...context, _component: component.id })
                  }}
                />
              ))}
            </section>
          )
        })}
      </div>
    </div>
  )
}

export function ManifestComponentRenderer({
  component,
  manifest,
  mode,
  resources,
  selected,
  onSelectComponent,
  onDuplicateComponent,
  onRemoveComponent,
  onAction,
}: {
  component: PageComponent
  manifest: PageManifest
  mode: ManifestRenderMode
  resources: Record<string, ManifestResourceState>
  selected?: boolean
  onSelectComponent?: (id: string | null) => void
  onDuplicateComponent?: (component: PageComponent) => void
  onRemoveComponent?: (id: string) => void
  onAction?: (actionId: string, context: Record<string, unknown>) => unknown | Promise<unknown>
}) {
  const entry = resolveComponentEntry(component.component, manifest.spec.capabilities)
  const actionsById = useMemo(() => new Map(manifest.spec.actions.map((action) => [action.id, action])), [manifest.spec.actions])
  const registeredComponent = useMemo(() => pageComponentToRegisteredConfig(component, actionsById), [component, actionsById])
  const resource = resolveComponentResource(component, manifest.spec.resources, resources)
  const disabled = mode !== 'runtime'
  const showChrome = mode === 'editor'

  const children = component.children?.map((child) => (
    <ManifestComponentRenderer
      key={child.id}
      component={child}
      manifest={manifest}
      mode={mode}
      resources={resources}
      selected={false}
      onSelectComponent={onSelectComponent}
      onDuplicateComponent={onDuplicateComponent}
      onRemoveComponent={onRemoveComponent}
      onAction={onAction}
    />
  ))

  const Component = entry?.component

  const body = !entry || !Component ? (
    <UnsupportedComponent config={registeredComponent} onAction={() => {}} />
  ) : shouldRenderResourceState(resource) ? (
    <ResourceStateBlock state={resource} />
  ) : (
    <Suspense fallback={<ComponentLoading />}>
      <Component
        config={registeredComponent}
        data={resource?.data}
        isLoading={resource?.kind === 'loading'}
        disabled={disabled}
        readonly={disabled}
        onAction={(actionId, context) => {
          if (disabled) return
          return onAction?.(actionId, context)
        }}
      >
        {children}
      </Component>
    </Suspense>
  )

  if (!showChrome) return <div data-component-id={component.id}>{body}</div>

  return (
    <div
      data-component-id={component.id}
      data-component-type={component.component}
      className={cn(
        'group relative rounded-xl border-2 p-2 transition-colors',
        selected ? 'border-primary bg-primary/5 ring-2 ring-primary/20' : 'border-transparent hover:border-primary/40',
      )}
      role="button"
      tabIndex={0}
      aria-pressed={selected}
      onClick={(event) => {
        event.stopPropagation()
        onSelectComponent?.(component.id)
      }}
      onKeyDown={(event) => {
        if (event.key === 'Enter' || event.key === ' ') {
          event.preventDefault()
          onSelectComponent?.(component.id)
        }
      }}
    >
      <div className="pointer-events-none absolute left-3 top-3 z-10 flex items-center gap-2 opacity-0 transition-opacity group-hover:opacity-100 group-focus:opacity-100">
        <Badge variant="secondary" className="font-mono text-[10px] shadow-sm">{component.component}@{component.version}</Badge>
        {component.data && <Badge variant="outline" className="font-mono text-[10px] shadow-sm">{component.data}</Badge>}
      </div>
      <div className="absolute right-3 top-3 z-20 flex items-center gap-1 opacity-0 transition-opacity group-hover:opacity-100 group-focus:opacity-100">
        <Button
          type="button"
          size="sm"
          variant="secondary"
          className="h-7 px-2 text-[11px]"
          onClick={(event) => {
            event.stopPropagation()
            onDuplicateComponent?.(component)
          }}
        >
          Duplicate
        </Button>
        <Button
          type="button"
          size="sm"
          variant="destructive"
          className="h-7 px-2 text-[11px]"
          onClick={(event) => {
            event.stopPropagation()
            onRemoveComponent?.(component.id)
          }}
        >
          Remove
        </Button>
      </div>
      <div className="min-h-10 rounded-lg bg-background/80">{body}</div>
    </div>
  )
}

function pageComponentToRegisteredConfig(
  component: PageComponent,
  actionsById: Map<string, PageManifest['spec']['actions'][number]> = new Map(),
): RegisteredComponentConfig {
  return {
    id: component.id,
    type: component.component,
    region: component.region,
    label: String(component.props.title || component.component.replace(/_/g, ' ')),
    source_doctype: String(component.props.source_doctype || ''),
    capabilities: component.required_capabilities,
    bindings: component.props.bindings as Record<string, string> | undefined,
    actions: component.actions?.map((id) => {
      const action = actionsById.get(id)
      return {
        id,
        trigger: 'on_click',
        type: action?.command || 'command',
        config: action?.input || {},
      }
    }),
    desktop_columns: component.props.desktop_columns as string[] | undefined,
    mobile_columns: component.props.mobile_columns as string[] | undefined,
    filters: component.props.filters as Array<{ field: string; op?: string; value?: unknown }> | undefined,
    components: component.children?.map((child) => pageComponentToRegisteredConfig(child, actionsById)),
    position: component.position,
    span: component.span,
  }
}

function applyResourceFilters(
  manifest: PageManifest,
  resources: Record<string, ManifestResourceState>,
  filters: Record<string, { field?: string; value?: string }>,
): Record<string, ManifestResourceState> {
  const next = { ...resources }
  for (const resource of manifest.spec.resources) {
    const active = filters[resource.id]
    const state = resources[resource.id]
    if (!active || !state?.data || !Array.isArray(state.data.data)) continue
    const rows = state.data.data as Array<Record<string, unknown>>
    const value = (active.value || '').trim().toLowerCase()
    const data = !value
      ? rows
      : active.field === '__search__'
        ? rows.filter((row) => Object.values(row).some((entry) => String(entry ?? '').toLowerCase().includes(value)))
        : rows.filter((row) => String(row[active.field || ''] ?? '').toLowerCase() === value)
    next[resource.id] = { ...state, data: { ...state.data, data, meta: { ...state.data.meta, total: data.length } } }
  }
  return next
}

export function createSimulatedResourceState(
  manifest: PageManifest,
  kind: ResourceSimulationKind,
): Record<string, ManifestResourceState> {
  return Object.fromEntries(manifest.spec.resources.map((resource) => [resource.id, createResourceState(resource, kind)]))
}

function useManifestRuntimeResources(manifest: PageManifest, enabled: boolean): Record<string, ManifestResourceState> {
  const queries = useQueries({
    queries: manifest.spec.resources.map((resource) => ({
      queryKey: ['manifest-resource', manifest.metadata.name, resource.id, resource.query, resource.params],
      queryFn: () => fetchManifestResource(resource),
      enabled,
      staleTime: 30_000,
      placeholderData: keepPreviousData,
    })),
  })

  return Object.fromEntries(manifest.spec.resources.map((resource, index) => {
    const query = queries[index]
    if (!enabled) return [resource.id, createResourceState(resource, 'normal')]
    const policyError = resourcePolicyError(resource)
    if (policyError) return [resource.id, { kind: 'error', message: policyError }]
    if (query.isLoading) return [resource.id, createResourceState(resource, 'loading')]
    if (query.isError) return [resource.id, { kind: 'error', message: query.error instanceof Error ? query.error.message : 'Resource failed to load.' }]
    const data = query.data ?? { data: [], meta: { total: 0, doctype: String(resource.params.doctype || '') } }
    return [resource.id, { kind: Array.isArray(data.data) && data.data.length === 0 ? 'empty' : 'normal', data }]
  }))
}

async function fetchManifestResource(resource: PageResource) {
  if (!isAllowedResourceQuery(resource.query)) {
    throw new Error(`Unsupported resource query ${resource.query}.`)
  }
  if (resource.query === 'document.list') {
    const { fetchList } = await import('../../lib/api/resources')
    const doctype = String(resource.params.doctype || '')
    const limit = typeof resource.params.limit === 'number' ? resource.params.limit : 50
    if (!doctype) return { data: [], meta: { total: 0, doctype: '' } }
    return fetchList(doctype, { limit })
  }
  if (resource.query === 'analytics.insights') {
    const doctype = String(resource.params.doctype || '')
    if (!doctype) return {}
    return fetchInsights(doctype)
  }
  return { data: [], meta: { total: 0, query: resource.query } }
}

function createResourceState(resource: PageResource, kind: ResourceSimulationKind): ManifestResourceState {
  if (kind === 'loading') return { kind }
  if (kind === 'empty') return { kind, data: { data: [], meta: { total: 0, doctype: String(resource.params.doctype || '') } } }
  if (kind === 'error') return { kind, message: 'The resource failed to load.' }
  if (kind === 'permission_denied') return { kind, message: 'You do not have access to this data yet.' }
  if (kind === 'offline') return { kind, message: 'This data is unavailable while offline.' }
  if (kind === 'conflict') return { kind, message: 'This data has a conflict that needs review.' }
  const data = { data: sampleRows(resource), meta: { total: 2, doctype: String(resource.params.doctype || 'Record') } }
  if (kind === 'stale') return { kind, data, message: 'Showing stale data while the resource refreshes.' }
  return { kind: 'normal', data }
}

function sampleRows(resource: PageResource): unknown[] {
  const doctype = String(resource.params.doctype || 'Record')
  return [
    { name: `${doctype}-001`, status: 'Open', amount: 1200, updated_at: '2026-08-14' },
    { name: `${doctype}-002`, status: 'Ready', amount: 860, updated_at: '2026-08-13' },
  ]
}

function resolveComponentResource(
  component: PageComponent,
  resources: PageResource[],
  states: Record<string, ManifestResourceState>,
): ManifestResourceState | undefined {
  const id = component.data?.split('.')[0] || resources[0]?.id
  return id ? states[id] : undefined
}

function shouldRenderResourceState(state: ManifestResourceState | undefined): boolean {
  return !!state && ['error', 'permission_denied', 'offline', 'conflict', 'stale'].includes(state.kind)
}

function ResourceStateBlock({ state }: { state?: ManifestResourceState }) {
  const icon = state?.kind === 'permission_denied' ? ShieldAlert :
    state?.kind === 'offline' ? WifiOff :
      state?.kind === 'conflict' ? AlertTriangle :
        state?.kind === 'error' ? Ban :
          state?.kind === 'stale' ? RefreshCcw :
            CloudOff
  const Icon = icon
  return (
    <div className="rounded-lg border border-dashed bg-muted/20 p-6 text-center" role="status">
      <Icon className="mx-auto h-6 w-6 text-muted-foreground" />
      <p className="mt-2 text-sm font-medium">{resourceStateTitle(state?.kind)}</p>
      <p className="mt-1 text-xs text-muted-foreground">{state?.message || 'The resource is not ready.'}</p>
    </div>
  )
}

function ComponentLoading() {
  return (
    <div className="flex items-center justify-center p-8" role="status" aria-live="polite">
      <Loader2 className="h-5 w-5 animate-spin text-muted-foreground" />
    </div>
  )
}

function resourceStateTitle(kind?: ResourceSimulationKind): string {
  if (kind === 'permission_denied') return 'You do not have access to this yet'
  if (kind === 'offline') return 'Offline'
  if (kind === 'conflict') return 'Needs review'
  if (kind === 'error') return 'Could not load this block'
  if (kind === 'stale') return 'Refreshing'
  return 'Resource unavailable'
}

function layoutClass(layout: PageManifest['spec']['layout']['type']): string {
  switch (layout) {
    case 'two_panel':
      return 'grid gap-6 lg:grid-cols-[minmax(0,1fr)_360px]'
    case 'three_panel':
      return 'grid gap-4 lg:grid-cols-[260px_minmax(0,1fr)] xl:grid-cols-[280px_minmax(0,1fr)_280px]'
    case 'grid':
      return 'grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-12'
    case 'single':
    default:
      return 'space-y-4'
  }
}

function regionClass(layout: PageManifest['spec']['layout']['type'], region: string): string {
  if (layout === 'grid') return 'space-y-4 lg:col-span-12'
  if (layout === 'single') return 'space-y-4'
  if (layout === 'two_panel') return region === 'side' ? 'min-w-0 space-y-4' : 'min-w-0 space-y-4'
  return 'min-w-0 space-y-4'
}

function regionsForLayout(layout: PageManifest['spec']['layout']['type']): Array<{ id: string; label: string }> {
  if (layout === 'two_panel') return [{ id: 'main', label: 'Main content' }, { id: 'side', label: 'Sidebar' }]
  if (layout === 'three_panel') return [{ id: 'left', label: 'Left panel' }, { id: 'main', label: 'Main content' }, { id: 'right', label: 'Right panel' }]
  return [{ id: 'main', label: 'Content' }]
}
