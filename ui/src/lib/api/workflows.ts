import { api } from './client'

export type WorkflowGoal = 'notify' | 'approval' | 'status'
export interface WorkflowRef { definition_id: string; version: number; digest: string }
export interface WorkflowTrigger { kind: 'record.created' | 'record.updated'; table: string; watched_fields?: string[] }
export interface WorkflowAction { name: string; version: number; input_schema: string; input: Record<string, unknown>; next: string }
export interface WorkflowFieldCondition { field: string; operator: string; value?: string | number | boolean }
export interface WorkflowDefinition {
  id: string
  version: number
  entry: string
  trigger?: WorkflowTrigger
  steps: Array<Record<string, unknown>>
}
export interface WorkflowCapability {
  table: string
  state_field?: string
  initial_state?: string
  admin_role: string
  triggers: string[]
  fields: Array<{ name: string; label: string; type: string; options?: string }>
  actions: Array<{ name: string; version: number; class: string; input_schema: string }>
  approval_policies: string[]
  transitions: Array<{ action: string; from: string; to: string; allowed?: string; required_fields?: string[] }>
  blockers?: string[]
}
export interface WorkflowPreview {
  id: string
  definition_id: string
  definition_version: number
  status: 'succeeded' | 'failed'
  outcome?: string
  failure?: string
  steps: Array<{ step_id: string; kind: string; result: string }>
  created_at: string
}
export interface WorkflowInstance {
  id: string
  definition_id: string
  definition_version: number
  status: string
  current_step: string
  subject: { kind: string; id: string; revision: string }
  created_at: string
  updated_at: string
}
export interface WorkflowHistoryEntry { sequence: number; step_id: string; kind: string; status: string; detail?: string; created_at: string }

export const listWorkflowDefinitions = () => api.get<WorkflowRef[]>('/api/v2/workflow-definitions')
export const getWorkflowDefinition = (id: string, version: number) => api.get<{ definition: WorkflowDefinition; reference: WorkflowRef }>(`/api/v2/workflow-definitions/${encodeURIComponent(id)}/${version}`)
export const getWorkflowCapabilities = (table: string) => api.get<WorkflowCapability>(`/api/v2/workflow-capabilities/${encodeURIComponent(table)}`)
export const validateWorkflow = (definition: WorkflowDefinition) => api.post<{ valid: boolean; issues: Array<{ path: string; code: string; detail: string }> }>('/api/v2/workflow-definitions/validate', definition)
export const previewWorkflow = (definition: WorkflowDefinition, record: Record<string, unknown>) => api.post<WorkflowPreview>('/api/v2/workflow-definitions/preview', { definition, record })
export const publishWorkflow = (definition: WorkflowDefinition) => api.postWithHeaders<WorkflowRef>('/api/v2/workflow-definitions', definition, { 'Idempotency-Key': crypto.randomUUID() })
export const listWorkflowPreviews = () => api.get<WorkflowPreview[]>('/api/v2/workflow-previews', { limit: 25 })
export const listWorkflowInstances = () => api.get<WorkflowInstance[]>('/api/v2/workflow-instances')
export const getWorkflowHistory = (id: string) => api.get<WorkflowHistoryEntry[]>(`/api/v2/workflow-instances/${encodeURIComponent(id)}/history`)
