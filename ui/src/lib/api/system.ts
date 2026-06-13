import { api } from './client'
import type { NavigationConfig } from '@/types/api'
import type { DocTypeSchema, DocType, ReferenceInfo } from '@/types/kora'

export async function fetchNavigation(): Promise<NavigationConfig> {
  return api.get<NavigationConfig>('/api/system/navigation')
}

export async function fetchDoctypeSchema(
  doctype: string,
  state?: string,
): Promise<DocTypeSchema> {
  const params: Record<string, string | undefined> = {}
  if (state) params.state = state
  return api.get<DocTypeSchema>(`/api/system/doctype/${encodeURIComponent(doctype)}`, params)
}

// --- Admin: Doctypes ---

export async function fetchDoctypes(): Promise<DocType[]> {
  return api.get<DocType[]>('/api/system/doctypes')
}

export async function createDoctype(
  data: DocType,
  activate = false,
): Promise<{ doctype: DocType; version_id: string; version_num: number; status: string }> {
  const path = activate ? '/api/system/doctype' : '/api/system/doctype?activate=false'
  return api.post(path, data)
}

export async function updateDoctype(
  name: string,
  data: DocType,
  activate = false,
): Promise<{ doctype: DocType; version_id: string; version_num: number; status: string }> {
  const path = activate
    ? `/api/system/doctype/${encodeURIComponent(name)}`
    : `/api/system/doctype/${encodeURIComponent(name)}?activate=false`
  return api.put(path, data)
}

export async function deleteDoctype(name: string): Promise<{ message: string }> {
  return api.delete(`/api/system/doctype/${encodeURIComponent(name)}`)
}

export async function validateDoctype(data: DocType): Promise<DocType> {
  return api.post<DocType>('/api/system/doctype/validate', data)
}

export async function fetchDoctypeYaml(name: string): Promise<string> {
  const url = new URL(`/api/system/doctype/${encodeURIComponent(name)}?format=yaml`, window.location.origin)
  const response = await fetch(url.toString(), { credentials: 'same-origin' })
  if (!response.ok) throw new Error('Failed to fetch YAML')
  return response.text()
}

export async function fetchDoctypeReferences(name: string): Promise<ReferenceInfo[]> {
  return api.get<ReferenceInfo[]>(`/api/system/doctype/${encodeURIComponent(name)}/references`)
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
  return api.post<ActivationPreview>('/api/system/doctype/dry-run', data)
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

export async function activateVersion(id: string): Promise<{ message: string; status: string }> {
  return api.post(`/api/system/config/versions/${id}/activate`)
}

export async function discardVersion(id: string): Promise<{ message: string; status: string }> {
  return api.post(`/api/system/config/versions/${id}/discard`)
}

export async function rollbackVersion(id: string): Promise<{ message: string; status: string }> {
  return api.post(`/api/system/config/versions/${id}/rollback`)
}

// --- Admin: Import (FormData upload) ---

export async function importDoctypeYaml(file: File): Promise<DocType> {
  const formData = new FormData()
  formData.append('file', file)
  // Use fetch directly since the api client always sends JSON.
  const url = new URL('/api/system/config/import', window.location.origin)
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
  return api.get<Role[]>('/api/system/roles')
}

export async function createRole(data: Role): Promise<Role> {
  return api.post<Role>('/api/system/roles', data)
}

export async function updateRole(name: string, data: Role): Promise<Role> {
  return api.put<Role>(`/api/system/roles/${encodeURIComponent(name)}`, data)
}

export async function deleteRole(name: string): Promise<{ message: string; users_with_role: number }> {
  return api.delete(`/api/system/roles/${encodeURIComponent(name)}`)
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
  return api.get<Permission[]>('/api/system/permissions')
}

export async function savePermissions(permissions: Permission[]): Promise<{ message: string }> {
  return api.put<{ message: string }>('/api/system/permissions', permissions)
}

// --- Admin: Workflows ---

export interface WorkflowDef {
  name: string
  document_type: string
  is_active: boolean
  workflow_state_field: string
  states: WorkflowState[]
  transitions: WorkflowTransition[]
  notifications: WorkflowNotification[]
}

export interface WorkflowState {
  state: string
  doc_status: number
  allow_edit: string
  style: string
}

export interface WorkflowTransition {
  action: string
  from: string
  to: string
  allowed: string
  condition?: string
  require_fields?: string[]
}

export interface WorkflowNotification {
  event: string
  to_state?: string
  recipients: { field: string }[]
  subject: string
  message: string
}

export async function fetchWorkflows(): Promise<WorkflowDef[]> {
  return api.get<WorkflowDef[]>('/api/system/workflows')
}

export async function fetchWorkflow(doctype: string): Promise<WorkflowDef> {
  return api.get<WorkflowDef>(`/api/system/workflows/${encodeURIComponent(doctype)}`)
}

export async function saveWorkflow(data: WorkflowDef): Promise<WorkflowDef> {
  return api.post<WorkflowDef>('/api/system/workflows', data)
}

export async function deleteWorkflow(doctype: string): Promise<{ message: string }> {
  return api.delete(`/api/system/workflows/${encodeURIComponent(doctype)}`)
}
