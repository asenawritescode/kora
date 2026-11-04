import type { PageAction, PageResource } from '@/manifest/schema/page'

export const ALLOWED_RESOURCE_QUERIES = ['document.list', 'analytics.insights'] as const

export const ALLOWED_ACTION_COMMANDS = [
  'document.create',
  'document.update',
  'document.delete',
  'document.submit',
  'document.cancel',
  'create_record',
  'update_record',
  'delete_record',
  'navigate',
  'local_cart_add',
  'local_cart_remove',
  'local_state_set',
  'create_transaction',
  'initiate_external_operation',
  'validate_external_operation',
] as const

const DATA_DISPLAY_COMPONENTS = new Set([
  'record_table',
  'record_list',
  'record_cards',
  'record_form',
  'record_detail',
  'metric_card',
  'chart',
  'kanban_board',
  'calendar_view',
  'approval_queue',
  'product_grid',
  'category_tabs',
  'scanner_count',
  'document_preview',
  'recent_records',
  'public_form',
  'insights_panel',
])

const RESERVED_ROUTE_PREFIXES = [
  '/workspace/admin',
  '/workspace/auth',
  '/workspace/settings',
  '/api',
  '/console',
  '/s/',
]

const RESERVED_ROUTE_EXACT = new Set(['/workspace', '/workspace/'])
const SAFE_BINDING_PATH = /^[A-Za-z_][A-Za-z0-9_-]*(\.[A-Za-z_][A-Za-z0-9_-]*)*$/
const SAFE_MANIFEST_ROUTE = /^\/[A-Za-z0-9][A-Za-z0-9_-]*$/

export function normalizeManifestRoute(route: string): string {
  const trimmed = route.trim()
  if (!trimmed) return '/'
  return trimmed.startsWith('/') ? trimmed : `/${trimmed}`
}

export function manifestRouteToPageSegment(route: string): string {
  return normalizeManifestRoute(route).replace(/^\/+/, '')
}

export function isAllowedResourceQuery(query: string): query is typeof ALLOWED_RESOURCE_QUERIES[number] {
  return ALLOWED_RESOURCE_QUERIES.includes(query as typeof ALLOWED_RESOURCE_QUERIES[number])
}

export function isAllowedActionCommand(command: string): command is typeof ALLOWED_ACTION_COMMANDS[number] {
  return ALLOWED_ACTION_COMMANDS.includes(command as typeof ALLOWED_ACTION_COMMANDS[number])
}

export function isReservedManifestRoute(route: string): boolean {
  const normalized = normalizeManifestRoute(route)
  if (RESERVED_ROUTE_EXACT.has(normalized)) return true
  return RESERVED_ROUTE_PREFIXES.some((prefix) => normalized === prefix || normalized.startsWith(`${prefix}/`))
}

export function isSafeManifestRoute(route: string): boolean {
  return SAFE_MANIFEST_ROUTE.test(normalizeManifestRoute(route))
}

export function isSafeBindingPath(path: string): boolean {
  return SAFE_BINDING_PATH.test(path)
}

export function componentNeedsResource(component: string): boolean {
  return DATA_DISPLAY_COMPONENTS.has(component)
}

export function resourcePolicyError(resource: PageResource): string | null {
  if (!isAllowedResourceQuery(resource.query)) return `Unsupported resource query ${resource.query}.`
  if (resource.query === 'document.list' && typeof resource.params.doctype !== 'string') {
    return 'document.list resources require a string params.doctype.'
  }
  if (resource.query === 'document.list') {
    const limit = resource.params.limit
    if (limit !== undefined && (!Number.isInteger(limit) || Number(limit) < 1 || Number(limit) > 200)) {
      return 'document.list resources require params.limit between 1 and 200.'
    }
    const filters = resource.params.filters
    if (filters !== undefined) {
      if (!Array.isArray(filters) || filters.length > 20) return 'document.list params.filters must be an array of at most 20 filters.'
      for (const filter of filters) {
        if (!filter || typeof filter !== 'object' || Array.isArray(filter)) return 'document.list filters must be field/operator/value objects.'
        const item = filter as Record<string, unknown>
        if (typeof item.field !== 'string' || !/^[A-Za-z_][A-Za-z0-9_]*$/.test(item.field)) return 'document.list filters require safe field names.'
        if (!['=', '!=', '>', '>=', '<', '<=', 'like', 'not like', 'in', 'not in'].includes(String(item.op))) return 'document.list filters require supported operators.'
        if (typeof item.value === 'string' && item.value.length > 256) return 'document.list filter values must not exceed 256 characters.'
        if (!['string', 'number', 'boolean'].includes(typeof item.value) && !Array.isArray(item.value)) return 'document.list filter values must be scalar or arrays.'
        if (Array.isArray(item.value) && (item.value.length < 1 || item.value.length > 100 || item.value.some((value) => !['string', 'number', 'boolean'].includes(typeof value)))) return 'document.list filter arrays must contain 1 to 100 scalar values.'
      }
    }
    const search = resource.params.search
    if (search !== undefined && (typeof search !== 'string' || search.length > 128)) {
      return 'document.list params.search must be a string of at most 128 characters.'
    }
    const orderBy = resource.params.order_by
    if (orderBy !== undefined && (typeof orderBy !== 'string' || !/^[A-Za-z_][A-Za-z0-9_]*(\s+(ASC|DESC))?$/i.test(orderBy))) {
      return 'document.list params.order_by must be a field with optional ASC or DESC.'
    }
  }
  if (resource.query === 'analytics.insights' && typeof resource.params.doctype !== 'string') {
    return 'analytics.insights resources require a string params.doctype.'
  }
  return null
}

export function documentListParams(resource: PageResource) {
  const filters = Array.isArray(resource.params.filters)
    ? JSON.stringify(resource.params.filters.map((filter) => {
        const item = filter as { field: string; op: string; value: unknown }
        return [item.field, item.op, item.value]
      }))
    : undefined
  return {
    limit: typeof resource.params.limit === 'number' ? resource.params.limit : 50,
    filters,
    order_by: typeof resource.params.order_by === 'string' ? resource.params.order_by : undefined,
    fields: Array.isArray(resource.params.fields) && resource.params.fields.every((field) => typeof field === 'string')
      ? resource.params.fields as string[]
      : undefined,
    search: typeof resource.params.search === 'string' ? resource.params.search : undefined,
  }
}

export function documentListResourceWithFilter(
  resource: PageResource,
  active?: { field?: string; value?: string },
): PageResource {
  if (resource.query !== 'document.list' || !active) return resource

  const params = { ...resource.params }
  if (active.field === '__search__') {
    params.search = active.value?.trim() || ''
  } else if (active.value?.trim() && active.field) {
    if (params.filters !== undefined && !Array.isArray(params.filters)) return resource
    const configuredFilters = Array.isArray(params.filters) ? params.filters : []
    params.filters = [...configuredFilters, { field: active.field, op: '=', value: active.value.trim() }]
  }
  return { ...resource, params }
}

export function actionPolicyError(action: PageAction): string | null {
  if (!isAllowedActionCommand(action.command)) return `Unsupported action command ${action.command}.`
  return null
}
