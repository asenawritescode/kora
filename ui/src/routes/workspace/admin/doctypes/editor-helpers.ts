import type { DocType, Field } from '../../../../types/kora'

export type GenericEditorDraft = {
  reply: string
  fields: Array<[string, Field['fieldtype']]>
  screens: Array<'table' | 'form'>
}

export function buildGenericEditorDraft(name: string): GenericEditorDraft {
  return {
    reply: name ? `I drafted a general ${name} data object. Review the fields and adjust them to your process.` : 'I drafted a general data object. Review the fields and adjust them to your process.',
    fields: [['Title', 'Data'], ['Status', 'Select'], ['Date', 'Date'], ['Details', 'Text'], ['Notes', 'Text']],
    screens: ['table', 'form'],
  }
}

export function genericEditorDefaults(): { name: string; module: string } {
  return { name: 'Workspace Record', module: 'Workspace' }
}

export type DraftIssue = {
  field?: string
  message: string
}

const RESERVED_FIELDNAMES = new Set([
  'name',
  'owner',
  'creation',
  'modified',
  'modified_by',
  'doc_status',
  'idx',
  'parent',
  'parentfield',
  'parenttype',
])

export function validateWizardDoctype(form: DocType, doctypes: DocType[] = []): DraftIssue[] {
  const issues: DraftIssue[] = []
  const name = form.name.trim()
  const moduleName = form.module.trim()

  if (!name) issues.push({ field: 'name', message: 'Give the data object a clear name.' })
  if (!moduleName) issues.push({ field: 'module', message: 'Choose an area so users know where it belongs.' })

  if (name) {
    const normalizedName = titleCase(name)
    if (RESERVED_FIELDNAMES.has(normalizedName.toLowerCase())) {
      issues.push({ field: 'name', message: 'This name is reserved. Choose a different business term.' })
    }
    if (doctypes.some((doctype: DocType) => doctype.name.toLowerCase() === normalizedName.toLowerCase())) {
      issues.push({ field: 'name', message: 'A data object with this name already exists.' })
    }
  }

  const fieldNames = form.fields
    .map((field: DocType['fields'][number], index: number) => slugField(field.fieldname || field.label || `field_${index + 1}`))
    .filter(Boolean)
  if (fieldNames.length === 0) {
    issues.push({ field: 'fields', message: 'Add at least one field.' })
  }
  if (new Set(fieldNames).size !== fieldNames.length) {
    issues.push({ field: 'fields', message: 'Field names must be unique.' })
  }
  if (fieldNames.some((fieldname: string) => RESERVED_FIELDNAMES.has(fieldname))) {
    issues.push({ field: 'fields', message: 'One or more field names use a reserved system name.' })
  }

  return issues
}

export function slugField(value: string): string {
  return value.trim().toLowerCase().replace(/[^a-z0-9]+/g, '_').replace(/^_|_$/g, '') || 'field'
}

export function titleCase(value: string): string {
  return value
    .trim()
    .split(/\s+/)
    .filter(Boolean)
    .map((part) => part.charAt(0).toUpperCase() + part.slice(1))
    .join(' ')
}
