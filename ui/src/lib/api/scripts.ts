import { api } from './client'

export interface ScriptRecord {
  name: string
  site: string
  script_type: string
  doctype: string
  event: string
  method_path: string
  workflow_action: string
  schedule: string
  priority: number
  is_active: boolean
  run_as: string
  timeout_ms: number
  script: string
  compiled_at?: string
  compile_error?: string
  created_by: string
  updated_by: string
  creation: string
  modified: string
}

export interface ScriptExecution {
  id: string
  script_name: string
  script_type: string
  doctype: string
  docname: string
  event: string
  trigger_user: string
  duration_ms: number
  status: string
  error_message?: string
  logged_at: string
}

export interface ScriptCreateRequest {
  name: string
  script_type: string
  doctype?: string
  event?: string
  method_path?: string
  workflow_action?: string
  schedule?: string
  priority?: number
  run_as?: string
  timeout_ms?: number
  script: string
}

export interface ScriptUpdateRequest {
  script_type?: string
  doctype?: string
  event?: string
  method_path?: string
  workflow_action?: string
  schedule?: string
  priority?: number
  is_active?: boolean
  run_as?: string
  timeout_ms?: number
  script?: string
}

export function fetchScripts() {
  return api.get<ScriptRecord[]>('/api/v1/system/script')
}

export function fetchScript(name: string) {
  return api.get<ScriptRecord>(`/api/v1/system/script/${encodeURIComponent(name)}`)
}

export function createScript(data: ScriptCreateRequest) {
  return api.post<ScriptRecord>('/api/v1/system/script', data)
}

export function updateScript(name: string, data: ScriptUpdateRequest) {
  return api.put(`/api/v1/system/script/${encodeURIComponent(name)}`, data)
}

export function deleteScript(name: string) {
  return api.delete(`/api/v1/system/script/${encodeURIComponent(name)}`)
}

export function validateScript(script: string) {
  return api.post<{ valid: boolean; error?: string; warnings?: string[] }>(
    '/api/v1/system/script/_validate',
    { script }
  )
}

export function fetchScriptExecutions(name: string) {
  return api.get<ScriptExecution[]>(`/api/v1/system/script/${encodeURIComponent(name)}/executions`)
}
