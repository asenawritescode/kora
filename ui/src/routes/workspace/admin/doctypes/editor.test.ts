import { describe, expect, it } from 'vitest'
import { buildGenericEditorDraft, genericEditorDefaults, validateWizardDoctype } from './editor-helpers'
import type { DocType } from '../../../../types/kora'

function makeDocType(overrides: Partial<DocType> = {}): DocType {
  return {
    name: 'Customer Visit',
    module: 'Field Service',
    is_submittable: false,
    is_child_table: false,
    is_single: false,
    track_changes: false,
    title_field: 'title',
    search_fields: 'title',
    sort_field: 'modified',
    sort_order: 'DESC',
    description: '',
    fields: [
      {
        fieldname: 'title',
        fieldtype: 'Data',
        label: 'Title',
        options: '',
        reqd: true,
        unique: false,
        default: '',
        hidden: false,
        read_only: false,
        bold: false,
        in_list_view: true,
        in_standard_filter: false,
        search_index: true,
        description: '',
        depends_on: '',
        mandatory_depends_on: '',
        constraints: null,
        renamed_from: '',
      },
    ],
    ...overrides,
  }
}

describe('validateWizardDoctype', () => {
  it('flags missing basics and fields', () => {
    const issues = validateWizardDoctype(makeDocType({ name: '', module: '', fields: [] }))
    expect(issues.map((issue) => issue.message)).toEqual([
      'Give the data object a clear name.',
      'Choose an area so users know where it belongs.',
      'Add at least one field.',
    ])
  })

  it('flags reserved and duplicate field names', () => {
    const issues = validateWizardDoctype(makeDocType({
      fields: [
        { ...makeDocType().fields[0], fieldname: 'name', label: 'Name' },
        { ...makeDocType().fields[0], fieldname: 'name', label: 'Second name' },
      ],
    }))
    expect(issues.some((issue) => issue.message.includes('reserved system name'))).toBe(true)
    expect(issues.some((issue) => issue.message === 'Field names must be unique.')).toBe(true)
  })

  it('flags duplicate object names against existing doctypes', () => {
    const issues = validateWizardDoctype(makeDocType(), [makeDocType()])
    expect(issues.some((issue) => issue.message === 'A data object with this name already exists.')).toBe(true)
  })
})

describe('generic doctype editor defaults', () => {
  it('keeps the offline assistant independent of business verticals', () => {
    expect(buildGenericEditorDraft('Stock Item')).toEqual({
      reply: 'I drafted a general Stock Item data object. Review the fields and adjust them to your process.',
      fields: [['Title', 'Data'], ['Status', 'Select'], ['Date', 'Date'], ['Details', 'Text'], ['Notes', 'Text']],
      screens: ['table', 'form'],
    })
  })

  it('uses stable generic names for unspecific records', () => {
    expect(genericEditorDefaults()).toEqual({ name: 'Workspace Record', module: 'Workspace' })
  })
})
