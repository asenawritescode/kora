import type { WorkflowCapability, WorkflowDefinition, WorkflowGoal } from './api/workflows'

export type AutomationEditor = {
  id: string
  version: number
  title: string
  table: string
  trigger: 'record.created' | 'record.updated'
  watchedField: string
  goal: WorkflowGoal
  recipientField: string
  subject: string
  message: string
  extraEmails: Array<{ recipientField: string; subject: string; message: string }>
  transitionAction: string
  stateField: string
  initialState: string
  transitionFrom: string
  transitionTo: string
  transitionRoles: string[]
  requiredFields: string[]
  transitionCondition: string
  docStatus: number
  policyID: string
  conditionOn: boolean
  conditionField: string
  conditionOperator: string
  conditionValue: string
}

export const emptyAutomationEditor = (): AutomationEditor => ({
  id: '', version: 1, title: '', table: '', trigger: 'record.created', watchedField: '', goal: 'notify', recipientField: '',
  subject: 'A record needs your attention', message: 'A new record is ready for you to review.', transitionAction: '', policyID: '',
  extraEmails: [], stateField: '', initialState: '', transitionFrom: '', transitionTo: '', transitionRoles: [], requiredFields: [], transitionCondition: '', docStatus: 0,
  conditionOn: false, conditionField: '', conditionOperator: 'equals', conditionValue: '',
})

export function sampleFieldValue(fieldType: string): string | number | boolean {
  if (['Int', 'Float', 'Currency', 'Percent'].includes(fieldType)) return 0
  if (fieldType === 'Check') return false
  return ''
}

export function createAutomationSample(editor: AutomationEditor, capability: WorkflowCapability): Record<string, unknown> {
  const sample = Object.fromEntries([['name', 'SAMPLE-1'], ...capability.fields.map((field) => [field.name, sampleFieldValue(field.type)])]) as Record<string, unknown>
  if (editor.goal === 'status' && editor.stateField) {
    sample[editor.stateField] = editor.transitionFrom || editor.initialState
  }
  return sample
}

function slugID(title: string, table: string) {
  return `${(title || 'automation').toLowerCase().normalize('NFKD').replace(/[^a-z0-9]+/g, '_').replace(/^_|_$/g, '').slice(0, 32) || 'automation'}_${table.toLowerCase().replace(/[^a-z0-9]+/g, '_').slice(0, 24)}`
}

export function buildAutomationDefinition(editor: AutomationEditor, capability: WorkflowCapability | undefined): WorkflowDefinition {
  const gateID = 'check_record'
  const actionID = 'take_action'
  const doneID = 'finished'
  const skippedID = 'not_matched'
  const statusGuard = editor.goal === 'status' && Boolean(editor.stateField && editor.transitionFrom)
  const hasCondition = editor.conditionOn || statusGuard
  const entry = hasCondition ? gateID : actionID
  const steps: Array<Record<string, unknown>> = []
  if (hasCondition) {
    const conditionField = statusGuard ? editor.stateField : editor.conditionField
    const conditionOperator = statusGuard ? 'equals' : editor.conditionOperator
    const conditionValue = statusGuard ? editor.transitionFrom : editor.conditionValue
    const value = conditionValue.trim()
    let parsedValue: string | number | boolean = value
    const type = capability?.fields.find((field) => field.name === conditionField)?.type
    if (['Int', 'Float', 'Currency', 'Percent'].includes(type || '')) parsedValue = Number(value)
    if (type === 'Check') parsedValue = value.toLowerCase() === 'true'
    steps.push({ id: gateID, kind: 'condition', condition: { field: { field: conditionField, operator: conditionOperator, ...(conditionOperator.startsWith('is_') ? {} : { value: parsedValue }) }, if_true: actionID, if_false: skippedID } })
  }
  if (editor.goal === 'notify') {
    const firstExtraID = editor.extraEmails.length ? 'extra_email_1' : doneID
    steps.push({ id: actionID, kind: 'action', action: { name: 'email.send', version: 1, input_schema: 'kora.email.send', input: { recipient_field: editor.recipientField, subject: editor.subject, message: editor.message }, next: firstExtraID } })
    editor.extraEmails.forEach((email, index) => steps.push({ id: `extra_email_${index + 1}`, kind: 'action', action: { name: 'email.send', version: 1, input_schema: 'kora.email.send', input: { recipient_field: email.recipientField, subject: email.subject, message: email.message }, next: index + 1 < editor.extraEmails.length ? `extra_email_${index + 2}` : doneID } }))
    steps.push({ id: doneID, kind: 'end', end: { outcome: 'notification_sent' } })
  } else if (editor.goal === 'status') {
    steps.push({ id: actionID, kind: 'action', action: { name: 'record.update_status', version: 1, input_schema: 'kora.record.update_status', input: { state_field: editor.stateField, initial_state: editor.initialState, transition_action: editor.transitionAction, from: editor.transitionFrom, to: editor.transitionTo, allowed_roles: editor.transitionRoles, required_fields: editor.requiredFields, ...(editor.transitionCondition ? { condition: editor.transitionCondition } : {}), doc_status: editor.docStatus }, next: doneID } })
    steps.push({ id: doneID, kind: 'end', end: { outcome: 'status_updated' } })
  } else {
    const outcomeSteps = [
      ['approved', 'approve', 'approved'], ['rejected', 'reject', 'rejected'], ['returned', 'return_for_changes', 'returned_for_changes'],
      ['expired', '', 'expired'], ['cancelled', '', 'cancelled'], ['stale', '', 'stale'],
    ] as const
    const outcomes: Record<string, string> = {}
    for (const [suffix, response] of outcomeSteps.slice(0, 3)) outcomes[response] = `approval_${suffix}`
    steps.push({ id: actionID, kind: 'human', human: {
      request: { kind: 'approval', subject: 'instance_subject', title: editor.subject, message: editor.message, policy_id: editor.policyID, due_after_seconds: 604800 },
      outcomes, on_expired: 'approval_expired', on_cancelled: 'approval_cancelled', on_stale: 'approval_stale',
    } })
    for (const [suffix, , result] of outcomeSteps) steps.push({ id: `approval_${suffix}`, kind: 'end', end: { outcome: result } })
  }
  if (hasCondition) steps.push({ id: skippedID, kind: 'end', end: { outcome: 'condition_not_met' } })
  return {
    id: editor.id || slugID(editor.title, editor.table), version: editor.version, entry,
    trigger: { kind: editor.goal === 'status' ? 'record.updated' : editor.trigger, table: editor.table, ...(editor.goal === 'status' && editor.stateField ? { watched_fields: [editor.stateField] } : editor.trigger === 'record.updated' && editor.watchedField ? { watched_fields: [editor.watchedField] } : {}) }, steps,
  }
}
