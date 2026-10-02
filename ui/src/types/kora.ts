export interface DocType {
  name: string
  resource_name?: string
  module: string
  is_submittable: boolean
  is_child_table: boolean
  is_single: boolean
  track_changes: boolean
  title_field: string
  search_fields: string
  sort_field: string
  sort_order: string
  description: string
  fields: Field[]
  doc_constraints?: DocConstraint[]
  public_access?: PublicAccess
  status?: string  // "Active" | "Draft" — set by the API
}

export type FieldType =
  | 'Data' | 'Text' | 'Text Editor'
  | 'Int' | 'Float' | 'Currency' | 'Percent'
  | 'Check' | 'Date' | 'Time' | 'Datetime'
  | 'Select' | 'Link' | 'Dynamic Link' | 'User'
  | 'Table' | 'Attach' | 'Attach Image' | 'Attach Audio'
  | 'JSON' | 'Password'
  | 'Section Break' | 'Column Break' | 'Heading'

export interface Field {
  fieldname: string
  fieldtype: FieldType
  label: string
  options: string
  reqd: boolean
  unique: boolean
  default: string
  hidden: boolean
  read_only: boolean
  bold: boolean
  in_list_view: boolean
  in_standard_filter: boolean
  search_index: boolean
  description: string
  depends_on: string
  mandatory_depends_on: string
  constraints: Constraint[] | null
  renamed_from: string
  linked_field?: string
  computed?: string
  dependency_scope?: string
  accept?: string
}

export interface Constraint {
  type: string
  value?: any
  values?: string[]
  pattern?: string
  message: string
  condition?: string
  scope?: string
}

export interface DocConstraint {
  type: string
  description?: string
  predicate?: string
  condition?: string
  message: string
  value?: any
  require_fields?: string[]
  field?: string
  group_by?: string[]
  lhs?: string
  operator?: string
  rhs?: string
  fields?: string[]
  status_field?: string
  status_values?: string[]
  immutable_fields?: string[]
  constraints?: Constraint[]
  max?: number
}

export interface PublicFilter {
  field: string
  op: string
  value: any
}

export interface PublicAccess {
  enabled: boolean
  list: boolean
  read: boolean
  fields: string[]
  filters: PublicFilter[]
  sort_field: string
  sort_order: string
  max_limit: number
  cache_max_age: number
}

export interface WorkflowState {
  state: string
  doc_status: number
  allow_edit: string
  style: 'default' | 'warning' | 'success' | 'danger' | 'info'
}

export interface WorkflowTransition {
  action: string
  from: string
  to: string
  allowed: string
  condition?: string
  require_fields?: string[]
}

export interface Workflow {
  states: WorkflowState[]
  transitions: WorkflowTransition[]
  state_field: string
}

export interface PermissionMap {
  [operation: string]: boolean
}

export interface ReferenceInfo {
  doctype: string
  fieldname: string
  label: string
}

export interface DocTypeSchema {
  doctype: DocType
  status?: string  // "Active" | "Draft"
  workflow?: Workflow
  permissions: PermissionMap
  transitions?: WorkflowTransition[]
  referenced_by?: ReferenceInfo[]
}

export interface PackageMetadata {
  name: string
  version: string
  digest: string
  signature?: string
  engine_range?: string
  frontend_range?: string
  status: 'draft' | 'preview' | 'active' | 'retired'
}

export interface PageManifest {
  name: string
  route: string
  components: unknown[]
  metadata: PackageMetadata
}

export interface Document {
  name: string
  doc_status: number
  [field: string]: any
}
