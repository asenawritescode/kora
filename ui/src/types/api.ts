export interface ApiResponse<T = any> {
  data: T
  meta?: {
    config_version?: number
    doctype?: string
    total?: number
    capabilities_version?: string
  }
}

export interface ApiError {
  code?: string
  type?: string
  message: string
  field?: string
  details?: Record<string, unknown>
}

export interface ApiErrorResponse {
  error: ApiError | string | { errors: ApiError[] }
}

export interface CurrentUser {
  name: string
  email: string
  full_name: string
  roles: string[]
}

export interface NavigationConfig {
  modules: ModuleGroup[]
  views?: ViewNavItem[]
  branding: Branding
  user: UserInfo
  admin_capabilities: string[]
  capabilities_version?: string
  supported_capabilities?: string[]
}

export interface ModuleGroup {
  module: string
  label: string
  doctypes: DocTypeNavItem[]
}

export interface DocTypeNavItem {
  name: string
  resource_name: string
  label: string
  icon?: string
  is_child: boolean
}

export interface ViewNavItem {
  name: string
  label: string
  route: string
  type: string
  module: string
  icon?: string
}

export interface Branding {
  app_name: string
  primary_color: string
}

export interface UserInfo {
  name: string
  full_name: string
  email: string
  roles: string[]
}

export interface ListParams {
  limit?: number
  offset?: number
  order_by?: string
  filters?: string
  search?: string
  fields?: string[]
}

export interface ListResponse<T = any> {
  data: T[]
  meta: {
    doctype: string
    total: number
    next_cursor?: string | null
    has_more?: boolean
  }
}

export type OperationStatus = 'completed' | 'accepted' | 'rejected' | 'conflict' | 'failed'

export interface OperationError {
  code: string
  message: string
  field?: string
  retryable?: boolean
}

export interface OperationEnvelope<T = any> {
  operation_id: string
  correlation_id?: string
  status: OperationStatus
  data?: T
  error?: OperationError
  next_cursor?: string | null
  has_more?: boolean
}

export interface RealtimeConnectionState {
  state: 'connecting' | 'connected' | 'degraded' | 'reconnecting' | 'offline' | 'unauthorized' | 'closed'
  detail?: string
  last_seen_at?: string
}

export interface CapabilitySnapshot {
  version: string
  capabilities: string[]
  offline?: {
    enabled: boolean
    mode?: 'unsupported' | 'read_only' | 'queue_writes' | 'full_slice'
  }
  frontend_runtime?: string
}

export interface ConsoleSite {
  name: string
  domains: string[]
  file_storage?: string
  storage_bucket?: string
  doctypes: number
  status: string
}
