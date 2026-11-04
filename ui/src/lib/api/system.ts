import { api } from './client'
import { getSitePrefix } from '../basepath'
import type { NavigationConfig } from '@/types/api'
import type { DocTypeSchema, DocType, ReferenceInfo } from '@/types/kora'

export async function fetchNavigation(): Promise<NavigationConfig> {
  return api.get<NavigationConfig>('/api/v1/system/navigation')
}

export function navigationQueryKey() {
  return ['navigation', getSitePrefix() || window.location.host] as const
}

export async function fetchDoctypeSchema(doctype: string): Promise<DocTypeSchema> {
  return api.get<DocTypeSchema>(`/api/v1/system/doctype/${encodeURIComponent(doctype)}`)
}

// --- Admin: Doctypes ---

export async function fetchDoctypes(): Promise<DocType[]> {
  return api.get<DocType[]>('/api/v1/system/doctypes')
}

export async function createDoctype(
  data: DocType,
  activate = false,
): Promise<{ doctype: DocType; version_id: string; version_num: number; status: string }> {
  const path = activate ? '/api/v1/system/doctype' : '/api/v1/system/doctype?activate=false'
  return api.post(path, data)
}

export async function updateDoctype(
  name: string,
  data: DocType,
  activate = false,
): Promise<{ doctype: DocType; version_id: string; version_num: number; status: string }> {
  const path = activate
    ? `/api/v1/system/doctype/${encodeURIComponent(name)}`
    : `/api/v1/system/doctype/${encodeURIComponent(name)}?activate=false`
  return api.put(path, data)
}

export async function deleteDoctype(name: string): Promise<{ message: string }> {
  return api.delete(`/api/v1/system/doctype/${encodeURIComponent(name)}`)
}

export async function validateDoctype(data: DocType): Promise<DocType> {
  return api.post<DocType>('/api/v1/system/doctype/validate', data)
}

export async function fetchDoctypeYaml(name: string): Promise<string> {
  const url = new URL(`/api/v1/system/doctype/${encodeURIComponent(name)}?format=yaml`, window.location.origin)
  const response = await fetch(url.toString(), { credentials: 'same-origin' })
  if (!response.ok) throw new Error('Failed to fetch YAML')
  return response.text()
}

export async function fetchDoctypeReferences(name: string): Promise<ReferenceInfo[]> {
  return api.get<ReferenceInfo[]>(`/api/v1/system/doctype/${encodeURIComponent(name)}/references`)
}

// --- Admin: Dry-Run ---

export interface TieredChange {
  tier: 'safe' | 'warning' | 'blocked'
  doctype: string
  field?: string
  change: string
  rows: number
  ddl?: string
  message: string
}

export interface ActivationPreview {
  ddl: string[]
  changes: TieredChange[]
  blocked: TieredChange[]
  warnings: TieredChange[]
}

export async function dryRunDoctype(data: DocType): Promise<ActivationPreview> {
  return api.post<ActivationPreview>('/api/v1/system/doctype/dry-run', data)
}

// --- Admin: Config Versions ---

export interface ConfigVersion {
  id: string
  site: string
  version: number
  created_at: string
  created_by: string
  label: string
  status: 'Draft' | 'Active' | 'Superseded'
}

export function isImmutableConfigVersion(version: Pick<ConfigVersion, 'status'>): boolean {
  return version.status !== 'Draft'
}

export function selectRollbackTargetVersion(versions: ConfigVersion[]): ConfigVersion | null {
  const immutable = versions.filter(isImmutableConfigVersion)
  return immutable.sort((a, b) => b.version - a.version)[0] ?? null
}

export interface ConfigVersionPreview {
  version_id: string
  status: string
  doctypes_in_snapshot: number
  roles_in_snapshot: number
  permissions_in_snapshot: number
  diff_summary: string
  is_breaking: boolean
  newer_active_versions: number
  section_changes?: unknown
  change_list_version: string
  warning?: string
}

export interface RollbackVersionPreview {
  version_id: string
  status: string
  doctypes_in_snapshot: number
  diff_summary: string
  is_breaking: boolean
  would_remove_doctypes: string[]
  changes: number
  warning?: string
}

export async function fetchConfigVersionPreview(id: string): Promise<ConfigVersionPreview> {
  return api.get(`/api/v1/system/config/versions/${id}/preview`)
}

export async function fetchRollbackVersionPreview(id: string): Promise<RollbackVersionPreview> {
  return api.get(`/api/v1/system/config/versions/${id}/rollback-preview`)
}

export async function activateVersion(id: string): Promise<{ message: string; status: string }> {
  return api.post(`/api/v1/system/config/versions/${id}/activate`)
}

export async function discardVersion(id: string): Promise<{ message: string; status: string }> {
  return api.post(`/api/v1/system/config/versions/${id}/discard`)
}

const rollbackIdempotencyKeys = new Map<string, string>()

function rollbackKeyStorageKey(id: string): string {
  const scope = typeof window === 'undefined' ? '' : getSitePrefix() || window.location.host
  return `kora.config-rollback-key.v1.${scope}.${id}`
}

function rollbackIdempotencyKey(id: string, requestedKey?: string): string {
  if (requestedKey) return requestedKey
  const key = rollbackKeyStorageKey(id)
  const cached = rollbackIdempotencyKeys.get(key)
  if (cached) return cached
  let stored = ''
  try { stored = window.sessionStorage.getItem(key) || '' } catch { /* Keep the retry key in memory for this tab. */ }
  const value = stored || crypto.randomUUID()
  rollbackIdempotencyKeys.set(key, value)
  try { window.sessionStorage.setItem(key, value) } catch { /* Server retries still use the in-memory key. */ }
  return value
}

function clearRollbackIdempotencyKey(id: string): void {
  const storageKey = rollbackKeyStorageKey(id)
  rollbackIdempotencyKeys.delete(storageKey)
  try { window.sessionStorage.removeItem(storageKey) } catch { /* The completed operation is safe to retry. */ }
}

export async function rollbackVersion(id: string, requestedKey?: string): Promise<{ message: string; status: string; operation_id?: string }> {
  const key = rollbackIdempotencyKey(id, requestedKey)
  const operation = await api.operation<{ message: string; status: string; operation_id?: string }>(
    'POST', `/api/v1/system/config/versions/${id}/rollback`, undefined, undefined, key,
  )
  if (operation.status !== 'completed' || !operation.data) {
    throw new Error(operation.error?.message || 'Config rollback did not complete.')
  }
  clearRollbackIdempotencyKey(id)
  return operation.data
}

export interface VersionSnapshot {
  version_id: string
  version: number
  site: string
  label: string
  doctype_names: string[]
  doctype_count: number
  roles_count: number
  permissions_count: number
  workflows_count: number
  snapshot: Record<string, unknown>
  pack_files: Array<{ path: string; content: string; content_type: string }>
}

export async function fetchVersionSnapshot(id: string): Promise<VersionSnapshot> {
  return api.get(`/api/v1/system/config/versions/${id}/snapshot`)
}

// --- Admin: Import (FormData upload) ---

export async function importDoctypeYaml(file: File): Promise<DocType> {
  const formData = new FormData()
  formData.append('file', file)
  // Use fetch directly since the api client always sends JSON.
  const url = new URL('/api/v1/system/config/import', window.location.origin)
  const csrfMatch = document.cookie.match(/(?:^|;\s*)kora_csrf=([^;]*)/)
  const csrf = csrfMatch ? decodeURIComponent(csrfMatch[1]) : ''
  const response = await fetch(url.toString(), {
    method: 'POST',
    credentials: 'same-origin',
    headers: csrf ? { 'X-Kora-CSRF-Token': csrf } : {},
    body: formData,
  })
  if (!response.ok) {
    const err = await response.json().catch(() => ({ error: 'Upload failed' }))
    throw new Error(typeof err.error === 'string' ? err.error : err.error?.message || 'Upload failed')
  }
  const json = await response.json()
  return json.data
}

// --- Admin: Roles ---

export interface Role {
  name: string
  workspace_access: boolean
  description: string
}

export async function fetchRoles(): Promise<Role[]> {
  return api.get<Role[]>('/api/v1/system/roles')
}

export async function createRole(data: Role): Promise<Role> {
  return api.post<Role>('/api/v1/system/roles', data)
}

export async function updateRole(name: string, data: Role): Promise<Role> {
  return api.put<Role>(`/api/v1/system/roles/${encodeURIComponent(name)}`, data)
}

export async function deleteRole(name: string): Promise<{ message: string; users_with_role: number }> {
  return api.delete(`/api/v1/system/roles/${encodeURIComponent(name)}`)
}

// --- Admin: Permissions ---

export interface Permission {
  doctype: string
  role: string
  read: boolean
  write: boolean
  create: boolean
  delete: boolean
  submit: boolean
  cancel: boolean
  amend: boolean
  export: boolean
  import: boolean
  report: boolean
  if_owner: boolean
}

export async function fetchPermissions(): Promise<Permission[]> {
  return api.get<Permission[]>('/api/v1/system/permissions')
}

export async function savePermissions(permissions: Permission[]): Promise<{ message: string }> {
  return api.put<{ message: string }>('/api/v1/system/permissions', permissions)
}

// --- Admin: Users ---

export interface User {
  name: string
  email: string
  full_name: string
  roles: string[]
  enabled: boolean
  created: string
  modified: string
}

export interface UserDirectoryEntry {
  name: string
  email: string
  full_name: string
}

export async function fetchUserDirectory(params?: { search?: string; limit?: number }): Promise<UserDirectoryEntry[]> {
  return api.get<UserDirectoryEntry[]>('/api/v1/system/user-directory', {
    q: params?.search,
    limit: params?.limit,
  })
}

export async function fetchUserDirectoryByNames(names: string[]): Promise<UserDirectoryEntry[]> {
  if (names.length === 0) return []
  return api.get<UserDirectoryEntry[]>('/api/v1/system/user-directory', { names: names.join(',') })
}

export interface UserUpdateRequest {
  roles?: string[]
  enabled?: boolean
}

export async function fetchUsers(): Promise<User[]> {
  return api.get<User[]>('/api/v1/system/users')
}

export async function updateUser(name: string, data: UserUpdateRequest): Promise<User> {
  return api.put<User>(`/api/v1/system/users/${encodeURIComponent(name)}`, data)
}

// --- Admin: Secrets ---

export interface SecretEntry {
  key_name: string
  updated_at: string
}

export interface SecretSetRequest {
  key: string
  value: string
}

export async function fetchSecrets(): Promise<SecretEntry[]> {
  return api.get<SecretEntry[]>('/api/v1/system/secrets')
}

export async function setSecret(data: SecretSetRequest): Promise<{ message: string; key: string }> {
  return api.post('/api/v1/system/secrets', data)
}

export async function deleteSecret(key: string): Promise<{ message: string; key: string }> {
  return api.delete(`/api/v1/system/secrets/${encodeURIComponent(key)}`)
}
