import type { PageAction, PageResource } from '@/manifest/schema/page'

export const ALLOWED_RESOURCE_QUERIES = ['document.list', 'analytics.insights'] as const

export const ALLOWED_ACTION_COMMANDS = [
  'document.create',
  'document.update',
  'document.delete',
  'document.submit',
  'document.cancel',
  'workflow.transition',
  'create_record',
  'update_record',
  'delete_record',
  'navigate',
  'workflow_transition',
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
  if (resource.query === 'analytics.insights' && typeof resource.params.doctype !== 'string') {
    return 'analytics.insights resources require a string params.doctype.'
  }
  return null
}

export function actionPolicyError(action: PageAction): string | null {
  if (!isAllowedActionCommand(action.command)) return `Unsupported action command ${action.command}.`
  return null
}
