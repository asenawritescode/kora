export interface ApiResponse<T = any> {
  data: T
  meta?: {
    config_version?: number
    doctype?: string
    total?: number
  }
}

export interface ApiError {
  type?: string
  message: string
  field?: string
}

export interface ApiErrorResponse {
  error: ApiError | { errors: ApiError[] }
}

export interface AuthProvider {
  name: string
  label: string
}

export interface CurrentUser {
  name: string
  email: string
  full_name: string
  roles: string[]
}

export interface LoginRequest {
  email: string
  password: string
}

export interface NavigationConfig {
  modules: ModuleGroup[]
  branding: Branding
  user: UserInfo
}

export interface ModuleGroup {
  module: string
  label: string
  doctypes: DocTypeNavItem[]
}

export interface DocTypeNavItem {
  name: string
  label: string
  icon?: string
  is_child: boolean
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
  fields?: string[]
}

export interface ListResponse<T = any> {
  data: T[]
  meta: {
    doctype: string
    total: number
  }
}

export interface ConsoleSite {
  name: string
  domains: string[]
  doctypes: number
  status: string
}
