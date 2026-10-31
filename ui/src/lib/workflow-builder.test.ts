import { describe, expect, it } from 'vitest'
import type { WorkflowCapability } from './api/workflows'
import { buildAutomationDefinition, createAutomationSample, emptyAutomationEditor } from './workflow-builder'

const capability: WorkflowCapability = {
  table: 'Purchase Request',
  state_field: 'status',
  admin_role: 'Administrator',
  triggers: ['record.created', 'record.updated'],
  fields: [
    { name: 'status', label: 'Status', type: 'Select', options: 'Draft\nPending\nApproved' },
    { name: 'amount', label: 'Amount', type: 'Currency' },
    { name: 'owner_email', label: 'Owner email', type: 'Email' },
  ],
  actions: [
    { name: 'email.send', version: 1, class: 'external', input_schema: 'kora.email.send' },
    { name: 'record.update_status', version: 1, class: 'local', input_schema: 'kora.record.update_status' },
  ],
  approval_policies: ['purchase-review'],
  transitions: [{ action: 'Submit', from: 'Draft', to: 'Pending', allowed: 'Administrator', required_fields: ['amount'] }],
}

describe('automation workflow builder', () => {
  it('watches the state field and guards a status transition against re-entry', () => {
    const editor = { ...emptyAutomationEditor(), title: 'Submit request', table: capability.table, goal: 'status' as const, transitionAction: 'Submit', stateField: 'status', initialState: 'Draft', transitionFrom: 'Draft', transitionTo: 'Pending', transitionRoles: ['Administrator'] }
    const definition = buildAutomationDefinition(editor, capability)
    expect(definition.trigger).toEqual({ kind: 'record.updated', table: capability.table, watched_fields: ['status'] })
    expect(definition.entry).toBe('check_record')
    const guard = definition.steps.find((step) => step.id === 'check_record') as any
    expect(guard.condition.field).toEqual({ field: 'status', operator: 'equals', value: 'Draft' })
    expect(guard.condition.if_false).toBe('not_matched')
    expect(createAutomationSample(editor, capability).status).toBe('Draft')
  })

  it('builds an email action behind a typed field condition', () => {
    const editor = {
      ...emptyAutomationEditor(), table: capability.table, goal: 'notify' as const, recipientField: 'owner_email',
      conditionOn: true, conditionField: 'amount', conditionOperator: 'at_least', conditionValue: '5000',
    }
    const definition = buildAutomationDefinition(editor, capability)
    const gate = definition.steps.find((step) => step.id === 'check_record') as any
    const action = definition.steps.find((step) => step.id === 'take_action') as any
    expect(gate.condition.field).toEqual({ field: 'amount', operator: 'at_least', value: 5000 })
    expect(gate.condition.if_true).toBe('take_action')
    expect(action.action).toMatchObject({ name: 'email.send', input_schema: 'kora.email.send', next: 'finished' })
  })

  it('routes all approval responses and lifecycle outcomes to visible end nodes', () => {
    const editor = { ...emptyAutomationEditor(), table: capability.table, goal: 'approval' as const, policyID: 'purchase-review' }
    const definition = buildAutomationDefinition(editor, capability)
    const approval = definition.steps.find((step) => step.id === 'take_action') as any
    expect(approval.human.request.policy_id).toBe('purchase-review')
    expect(Object.keys(approval.human.outcomes).sort()).toEqual(['approve', 'reject', 'return_for_changes'])
    for (const target of [...Object.values(approval.human.outcomes), approval.human.on_expired, approval.human.on_cancelled, approval.human.on_stale]) {
      expect(definition.steps.some((step) => step.id === target && step.kind === 'end')).toBe(true)
    }
  })

  it('builds a status transition as a triggered registered action', () => {
    const editor = { ...emptyAutomationEditor(), title: 'Request status', table: capability.table, goal: 'status' as const, stateField: 'status', initialState: 'Draft', transitionAction: 'Submit', transitionFrom: 'Draft', transitionTo: 'Pending', transitionRoles: ['Administrator'], requiredFields: ['amount'] }
    const definition = buildAutomationDefinition(editor, capability)
    expect(definition.trigger).toEqual({ kind: 'record.updated', table: capability.table, watched_fields: ['status'] })
    const action = definition.steps.find((step) => step.id === 'take_action') as any
    expect(action.action.input).toMatchObject({ state_field: 'status', initial_state: 'Draft', transition_action: 'Submit', from: 'Draft', to: 'Pending', allowed_roles: ['Administrator'], required_fields: ['amount'] })
  })

  it('chains additional email steps in order', () => {
    const editor = { ...emptyAutomationEditor(), table: capability.table, goal: 'notify' as const, recipientField: 'owner_email', extraEmails: [
      { recipientField: 'owner_email', subject: 'Second notice', message: 'Follow up' },
    ] }
    const definition = buildAutomationDefinition(editor, capability)
    const first = definition.steps.find((step) => step.id === 'take_action') as any
    const second = definition.steps.find((step) => step.id === 'extra_email_1') as any
    expect(first.action.next).toBe('extra_email_1')
    expect(second.action.next).toBe('finished')
  })
})
