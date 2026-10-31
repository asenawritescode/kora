import { useNavigate, useRouterState } from '@tanstack/react-router'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useState, useCallback, useRef, useEffect } from 'react'
import { fetchDoctypes, createDoctype, updateDoctype } from '@/lib/api/system'
import { createPageManifest } from '@/lib/api/page-manifests'
import { dryRunDoctype } from '@/lib/api/system'
import type { Constraint, DocConstraint, DocType, Field, PublicAccess, PublicFilter } from '@/types/kora'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Switch } from '@/components/ui/switch'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Separator } from '@/components/ui/separator'
import { Skeleton } from '@/components/ui/skeleton'
import { Textarea } from '@/components/ui/textarea'
import { ArrowLeft, Plus, ChevronDown, ChevronRight, GripVertical, Edit, Trash2, Save } from 'lucide-react'
import { YamlPanel } from '@/components/forms/YamlPanel'
import { LispAutocomplete } from '@/components/forms/LispAutocomplete'
import { Badge } from '@/components/ui/badge'
import { FieldLabelWithHelp, HelpTooltip } from '@/components/ui/help-tooltip'
import { cn } from '@/lib/utils'
import { createStandardPageManifest, STANDARD_PAGE_KINDS, type StandardPageKind } from '@/manifest/runtime/standard-pages'
import { buildGenericEditorDraft, genericEditorDefaults, validateWizardDoctype, titleCase, slugField, type DraftIssue } from './editor-helpers'

// --- Field type groups ---
const FIELD_TYPE_GROUPS: { label: string; types: string[] }[] = [
  { label: 'Text', types: ['Data', 'Text', 'Text Editor', 'Password', 'JSON'] },
  { label: 'Numbers', types: ['Int', 'Float', 'Currency', 'Percent'] },
  { label: 'Date & Time', types: ['Date', 'Time', 'Datetime'] },
  { label: 'Selection', types: ['Select', 'Check'] },
  { label: 'Relations', types: ['Link', 'Dynamic Link', 'Table'] },
  { label: 'Files', types: ['Attach', 'Attach Image', 'Attach Audio'] },
  { label: 'Layout', types: ['Section Break', 'Column Break', 'Heading'] },
]

const EMPTY_FIELD: Field = {
  fieldname: '',
  fieldtype: 'Data' as any,
  label: '',
  options: '',
  reqd: false,
  unique: false,
  default: '',
  hidden: false,
  read_only: false,
  bold: false,
  in_list_view: false,
  in_standard_filter: false,
  search_index: false,
  description: '',
  depends_on: '',
  mandatory_depends_on: '',
  constraints: null,
  renamed_from: '',
  linked_field: '',
  computed: '',
  dependency_scope: '',
  accept: '',
}

const EMPTY_PUBLIC_ACCESS = {
  enabled: false,
  list: false,
  read: false,
  fields: [] as string[],
  filters: [] as Array<{ field: string; op: string; value: any }>,
  sort_field: '',
  sort_order: 'DESC',
  max_limit: 50,
  cache_max_age: 60,
}

const PUBLIC_VISIBILITY_FIELDNAME = 'is_public'
const PUBLIC_VISIBILITY_FILTER: PublicFilter = {
  field: PUBLIC_VISIBILITY_FIELDNAME,
  op: 'equals',
  value: true,
}

const EMPTY_DOC_CONSTRAINT = {
  type: 'Predicate',
  description: '',
  predicate: '',
  condition: '',
  message: '',
  require_fields: [] as string[],
  field: '',
  group_by: [] as string[],
  lhs: '',
  operator: '',
  rhs: '',
  fields: [] as string[],
  status_field: '',
  status_values: [] as string[],
  immutable_fields: [] as string[],
  constraints: [] as Array<{ type: string; value?: any; values?: string[]; pattern?: string; message: string; condition?: string; scope?: string }>,
}

const EMPTY_DOCTYPE: DocType = {
  name: '',
  resource_name: '',
  module: '',
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
    { ...EMPTY_FIELD, fieldname: 'title', fieldtype: 'Data', label: 'Title', reqd: true, in_list_view: true, search_index: true },
  ],
  doc_constraints: [],
  public_access: { ...EMPTY_PUBLIC_ACCESS },
}

const WIZARD_STEPS = ['Basics', 'Fields', 'Advanced', 'Screens', 'Review'] as const
type WizardStep = typeof WIZARD_STEPS[number]
type AssistantDraft = {
  reply: string
  fields: Array<[string, Field['fieldtype']]>
  screens: StandardPageKind[]
}

const STARTER_TEMPLATES: Array<{ label: string; description: string; fields: Array<[string, Field['fieldtype']]> }> = [
  {
    label: 'Basic record',
    description: 'A title, status, and notes.',
    fields: [['Title', 'Data'], ['Status', 'Select'], ['Notes', 'Text']],
  },
  {
    label: 'Request record',
    description: 'A requester, date, status, and details.',
    fields: [['Title', 'Data'], ['Requester', 'Link'], ['Submitted date', 'Date'], ['Status', 'Select'], ['Details', 'Text']],
  },
  {
    label: 'Task tracker',
    description: 'Owner, priority, due date, and progress.',
    fields: [['Subject', 'Data'], ['Owner', 'Link'], ['Priority', 'Select'], ['Due date', 'Date'], ['Done', 'Check']],
  },
]

export default function AdminDoctypeEditorPage() {
  const navigate = useNavigate()
  const routerState = useRouterState()
  // Determine if editing from the URL path: .../doctypes/<name> vs .../doctypes/new
  const pathParts = routerState.location.pathname.replace(/\/$/, '').split('/')
  const lastSegment = pathParts[pathParts.length - 1]
  const isEdit = lastSegment !== 'new' && lastSegment !== 'doctypes'
  const doctypeName = isEdit ? decodeURIComponent(lastSegment) : undefined

  const [form, setForm] = useState<DocType>(EMPTY_DOCTYPE)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [expandedField, setExpandedField] = useState<number | null>(0)
  const [loadingEdit, setLoadingEdit] = useState(isEdit)
  const [currentStatus, setCurrentStatus] = useState<string | null>(null)
  const [wizardStep, setWizardStep] = useState<WizardStep>('Basics')
  const [selectedScreens, setSelectedScreens] = useState<StandardPageKind[]>(['table', 'form'])
  const [preflight, setPreflight] = useState<{
    status: 'idle' | 'loading' | 'ready' | 'error'
    result?: Awaited<ReturnType<typeof dryRunDoctype>>
    message?: string
  }>({ status: 'idle' })

  // For edit mode, load from the doctypes list.
  const { data: doctypes } = useQuery({
    queryKey: ['admin', 'doctypes'],
    queryFn: fetchDoctypes,
    enabled: true,
  })

  // Populate form when doctypes load in edit mode.
  useEffect(() => {
    if (isEdit && doctypes && loadingEdit) {
      const existing = doctypes.find((d: DocType) => d.name === doctypeName)
      if (existing) {
        setForm(JSON.parse(JSON.stringify(existing)))
        setCurrentStatus(existing.status || null)
        setLoadingEdit(false)
      }
    }
  }, [isEdit, doctypes, doctypeName, loadingEdit])

  const updateField = useCallback((index: number, updates: Partial<Field>) => {
    setForm((prev) => {
      const fields = [...prev.fields]
      fields[index] = { ...fields[index], ...updates }
      return { ...prev, fields }
    })
  }, [])

  const addField = useCallback(() => {
    setForm((prev) => ({
      ...prev,
      fields: [...prev.fields, { ...EMPTY_FIELD }],
    }))
    setExpandedField(form.fields.length)
  }, [form.fields.length])

  const removeField = useCallback((index: number) => {
    setForm((prev) => ({
      ...prev,
      fields: prev.fields.filter((_, i) => i !== index),
    }))
    setExpandedField(null)
  }, [])

  const moveField = useCallback((index: number, direction: -1 | 1) => {
    setForm((prev) => {
      const fields = [...prev.fields]
      const target = index + direction
      if (target < 0 || target >= fields.length) return prev
      ;[fields[index], fields[target]] = [fields[target], fields[index]]
      return { ...prev, fields }
    })
    setExpandedField(null)
  }, [])

  const queryClient = useQueryClient()

  const handleSave = async (activate: boolean) => {
    setSaving(true)
    setError(null)
    try {
      if (activate) {
        const preview = await dryRunDoctype(normalizeDoctypeForSave(form))
        setPreflight({ status: 'ready', result: preview })
        if (preview.blocked.length > 0) {
          setError(formatPreviewBlockers(preview.blocked))
          return
        }
      }

      const normalized = normalizeDoctypeForSave(form)
      if (isEdit) {
        await updateDoctype(doctypeName!, normalized, activate)
      } else {
        await createDoctype(normalized, activate)
      }
      // Invalidate caches so the new/updated doctype appears immediately
      // in the admin list, sidebar navigation, and dashboard.
      queryClient.invalidateQueries({ queryKey: ['admin', 'doctypes'] })
      queryClient.invalidateQueries({ queryKey: ['navigation'] })
      navigate({ to: '/workspace/admin/doctypes' })
    } catch (e) {
      setError((e as Error).message)
    } finally {
      setSaving(false)
    }
  }

  const handleWizardCreate = async () => {
    const validation = validateWizardDoctype(form, doctypes ?? [])
    if (validation.length > 0) {
      setError(validation.map((issue) => issue.message).join(' '))
      return
    }

    setSaving(true)
    setError(null)
    try {
      const normalized = normalizeWizardDoctype(form)
      await createDoctype(normalized, true)

      for (const screen of selectedScreens) {
        const manifest = createStandardPageManifest(normalized, screen)
        await createPageManifest(manifest)
      }

      queryClient.invalidateQueries({ queryKey: ['admin', 'doctypes'] })
      queryClient.invalidateQueries({ queryKey: ['page-manifests'] })
      queryClient.invalidateQueries({ queryKey: ['navigation'] })

      if (selectedScreens.length > 0) {
        const first = createStandardPageManifest(normalized, selectedScreens[0])
        navigate({ to: '/workspace/admin/page-manifests/$name', params: { name: first.metadata.name } })
      } else {
        navigate({ to: '/workspace/admin/doctypes' })
      }
    } catch (e) {
      setError((e as Error).message)
    } finally {
      setSaving(false)
    }
  }

  if (isEdit && loadingEdit) {
    return (
      <div className="p-8 space-y-4">
        <Skeleton className="h-8 w-64" />
        <Skeleton className="h-96 w-full" />
      </div>
    )
  }

  if (!isEdit) {
    return (
      <DoctypeCreationWizard
        form={form}
        setForm={setForm}
        step={wizardStep}
        setStep={setWizardStep}
        expandedField={expandedField}
        setExpandedField={setExpandedField}
        selectedScreens={selectedScreens}
        setSelectedScreens={setSelectedScreens}
        doctypes={doctypes ?? []}
        error={error}
        saving={saving}
        issues={validateWizardDoctype(form, doctypes ?? [])}
        onCancel={() => navigate({ to: '/workspace/admin/doctypes' })}
        onCreate={handleWizardCreate}
        onFieldChange={updateField}
        onFieldRemove={removeField}
        onFieldMove={moveField}
      />
    )
  }

  return (
    <div className="h-[calc(100vh-4rem)] flex flex-col overflow-hidden">
      {/* Header */}
      <div className="flex items-center justify-between px-3 sm:px-6 py-3 border-b shrink-0 gap-2">
        <div className="flex items-center gap-2 sm:gap-4 min-w-0">
            <Button variant="ghost" size="icon" className="h-8 w-8 shrink-0" aria-label="Back to DocTypes" title="Back to DocTypes" onClick={() => navigate({ to: '/workspace/admin/doctypes' })}>
            <ArrowLeft className="h-4 w-4" />
          </Button>
          <h1 className="text-base sm:text-2xl font-bold tracking-tight truncate">
            {isEdit ? doctypeName : 'New'}
          </h1>
          {currentStatus && (
            currentStatus === 'Active' ? (
              <Badge variant="default" className="bg-green-600 hover:bg-green-600 shrink-0">Active</Badge>
            ) : (
              <Badge variant="secondary" className="bg-amber-100 text-amber-800 hover:bg-amber-100 shrink-0">Draft</Badge>
            )
          )}
        </div>
        <div className="flex gap-1 sm:gap-2 shrink-0">
          <Button variant="outline" size="sm" className="text-xs sm:text-sm h-8" onClick={() => handleSave(false)} disabled={saving}>
            Save draft
          </Button>
          <Button size="sm" className="text-xs sm:text-sm h-8" onClick={() => handleSave(true)} disabled={saving}>
            <Save className="h-3.5 w-3.5 sm:mr-1" />
            <span className="hidden sm:inline">{currentStatus === 'Active' ? 'Save & Migrate' : 'Save & Activate'}</span>
          </Button>
        </div>
      </div>

      <div className="mx-4 mt-2 space-y-2">
        {error && (
          <div className="p-3 border border-destructive/50 bg-destructive/10 rounded-lg text-sm text-destructive">
            {error}
          </div>
        )}
        {preflight.status === 'ready' && preflight.result && (
          <PreflightSummary preview={preflight.result} />
        )}
      </div>

      {/* Split pane: Form (left) + YAML (right) */}
      <div className="flex-1 flex overflow-hidden">
        {/* Form panel */}
      <div className="flex-1 overflow-y-auto p-4 sm:p-6 lg:p-8 space-y-6 sm:space-y-8">
        {/* Doctype Properties */}
        <section>
          <h2 className="text-lg font-semibold mb-4">Basic details</h2>
          <div className="grid grid-cols-1 sm:grid-cols-2 gap-4">
            <div>
              <Label htmlFor="name">Data object name *</Label>
              <Input
                id="name"
                value={form.name}
                onChange={(e) => setForm({ ...form, name: e.target.value })}
                placeholder="Workspace Record"
                disabled={isEdit}
              />
            </div>
            <div>
              <Label htmlFor="module">Area *</Label>
              <Input
                id="module"
                value={form.module}
                onChange={(e) => setForm({ ...form, module: e.target.value })}
                placeholder="Billing"
              />
            </div>
            <div>
              <Label htmlFor="title_field">Main label field</Label>
              <Input
                id="title_field"
                value={form.title_field}
                onChange={(e) => setForm({ ...form, title_field: e.target.value })}
              />
            </div>
            <div>
              <Label htmlFor="search_fields">Searchable fields</Label>
              <Input
                id="search_fields"
                value={form.search_fields}
                onChange={(e) => setForm({ ...form, search_fields: e.target.value })}
                placeholder="name, email"
              />
            </div>
            <div>
              <Label htmlFor="sort_field">Sort by</Label>
              <Input
                id="sort_field"
                value={form.sort_field}
                onChange={(e) => setForm({ ...form, sort_field: e.target.value })}
              />
            </div>
            <div>
              <Label htmlFor="sort_order">Sort direction</Label>
              <Select value={form.sort_order} onValueChange={(v) => setForm({ ...form, sort_order: v || 'DESC' })}>
                <SelectTrigger>
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="ASC">ASC</SelectItem>
                  <SelectItem value="DESC">DESC</SelectItem>
                </SelectContent>
              </Select>
            </div>
          </div>
          <div className="grid grid-cols-2 sm:flex sm:flex-wrap gap-2 sm:gap-5 mt-4">
            <label className="flex items-center gap-2 text-sm">
              <Switch checked={form.is_submittable} onCheckedChange={(v) => setForm({ ...form, is_submittable: v })} />
              <span className="truncate">Needs approval</span>
            </label>
            <label className="flex items-center gap-2 text-sm">
              <Switch checked={form.is_child_table} onCheckedChange={(v) => setForm({ ...form, is_child_table: v })} />
              <span className="truncate">Belongs inside another record</span>
            </label>
            <label className="flex items-center gap-2 text-sm">
              <Switch checked={form.is_single} onCheckedChange={(v) => setForm({ ...form, is_single: v })} />
              <span className="truncate">Only one record</span>
            </label>
            <label className="flex items-center gap-2 text-sm">
              <Switch checked={form.track_changes} onCheckedChange={(v) => setForm({ ...form, track_changes: v })} />
              <span className="truncate">Keep change history</span>
            </label>
          </div>
        </section>

        <Separator />

        <PublicAccessEditor form={form} setForm={setForm} />

        <Separator />

        {/* Fields */}
        <section>
          <div className="flex items-center justify-between mb-4">
            <h2 className="text-lg font-semibold">Fields</h2>
            <Button variant="outline" size="sm" onClick={addField}>
              <Plus className="h-4 w-4 mr-1" /> Add Field
            </Button>
          </div>
          <p className="text-sm text-muted-foreground mb-3">Give each field a clear label. Kigae will keep the internal field key in sync for you.</p>

          <div className="space-y-1">
            {form.fields.map((field, index) => (
              <FieldRow
                key={index}
                field={field}
                index={index}
                expanded={expandedField === index}
                onToggle={() => setExpandedField(expandedField === index ? null : index)}
                onChange={(updates) => updateField(index, updates)}
                onRemove={() => removeField(index)}
                onMoveUp={() => moveField(index, -1)}
                onMoveDown={() => moveField(index, 1)}
                canMoveUp={index > 0}
                canMoveDown={index < form.fields.length - 1}
                allDoctypes={doctypes?.map((d: DocType) => d.name) || []}
              />
            ))}
          </div>
        </section>

        <Separator />

        {/* Document Constraints */}
        <section>
          <div className="flex items-center justify-between mb-4">
            <h2 className="text-lg font-semibold">Checks and rules</h2>
            <Button variant="outline" size="sm" onClick={() => {
              setForm({
                ...form,
                doc_constraints: [...(form.doc_constraints || []), { type: 'Predicate', predicate: '', condition: '', message: '' }]
              })
            }}>
              <Plus className="h-4 w-4 mr-1" /> Add Constraint
            </Button>
          </div>
          {(form.doc_constraints || []).length === 0 && (
            <p className="text-sm text-muted-foreground italic">No rules added yet.</p>
          )}
          {(form.doc_constraints || []).map((c, ci) => (
            <div key={ci} className="border rounded-lg p-3 mb-2">
              <div className="grid grid-cols-12 gap-2 items-start">
                <div className="col-span-3">
                      <Label className="text-xs">Rule type</Label>
                  <select
                    className="w-full h-9 rounded-md border bg-background px-2 text-sm"
                    value={c.type}
                    onChange={(e) => {
                      const updated = [...(form.doc_constraints || [])]
                      updated[ci] = { ...updated[ci], type: e.target.value }
                      setForm({ ...form, doc_constraints: updated })
                    }}
                  >
                    <option value="Predicate">Predicate</option>
                    <option value="max">max</option>
                    <option value="min">min</option>
                    <option value="max_length">max_length</option>
                    <option value="min_length">min_length</option>
                    <option value="regex">regex</option>
                    <option value="one_of">one_of</option>
                    <option value="not_one_of">not_one_of</option>
                    <option value="min_date">min_date</option>
                    <option value="max_date">max_date</option>
                  </select>
                </div>

                {c.type === 'Predicate' ? (
                  <>
                    <div className="col-span-4">
                      <Label className="text-xs">Rule expression</Label>
                      <LispAutocomplete
                        className="h-9 text-sm font-mono"
                        value={c.predicate || ''}
                        onChange={(val) => {
                          const updated = [...(form.doc_constraints || [])]
                          updated[ci] = { ...updated[ci], predicate: val }
                          setForm({ ...form, doc_constraints: updated })
                        }}
                        placeholder="(> end_date start_date)"
                        fieldNames={form.fields?.map((f: any) => f.fieldname) || []}
                      />
                    </div>
                    <div className="col-span-4">
                      <Label className="text-xs">Apply only when</Label>
                      <LispAutocomplete
                        className="h-9 text-sm font-mono"
                        value={c.condition || ''}
                        onChange={(val) => {
                          const updated = [...(form.doc_constraints || [])]
                          updated[ci] = { ...updated[ci], condition: val }
                          setForm({ ...form, doc_constraints: updated })
                        }}
                        placeholder='doc.type == "wholesale"'
                        fieldNames={form.fields?.map((f: any) => f.fieldname) || []}
                      />
                    </div>
                    <div className="col-span-1 flex items-end pb-1">
                      <Button
                        variant="ghost" size="sm"
                        className="h-9 text-destructive w-full"
                        onClick={() => {
                          const updated = (form.doc_constraints || []).filter((_, i) => i !== ci)
                          setForm({ ...form, doc_constraints: updated.length > 0 ? updated : undefined })
                        }}
                      >✕</Button>
                    </div>
                  </>
                ) : (
                  <>
                    <div className="col-span-3">
                      <Label className="text-xs">Rule value</Label>
                      <Input
                        className="h-9 text-sm"
                        value={c.value != null ? String(c.value) : ''}
                        onChange={(e) => {
                          const v = e.target.value
                          const num = Number(v)
                          const updated = [...(form.doc_constraints || [])]
                          updated[ci] = { ...updated[ci], value: isNaN(num) ? v : num }
                          setForm({ ...form, doc_constraints: updated })
                        }}
                        placeholder="value"
                      />
                    </div>
                    <div className="col-span-5">
                      <Label className="text-xs">User-facing message</Label>
                      <Input
                        className="h-9 text-sm"
                        value={c.message || ''}
                        onChange={(e) => {
                          const updated = [...(form.doc_constraints || [])]
                          updated[ci] = { ...updated[ci], message: e.target.value }
                          setForm({ ...form, doc_constraints: updated })
                        }}
                        placeholder="Error message"
                      />
                    </div>
                    <div className="col-span-1 flex items-end pb-1">
                      <Button
                        variant="ghost" size="sm"
                        className="h-9 text-destructive w-full"
                        onClick={() => {
                          const updated = (form.doc_constraints || []).filter((_, i) => i !== ci)
                          setForm({ ...form, doc_constraints: updated.length > 0 ? updated : undefined })
                        }}
                      >✕</Button>
                    </div>
                  </>
                )}
              </div>

              {c.type === 'Predicate' && (
                <div className="mt-2">
                  <Label className="text-xs">User-facing message</Label>
                  <Input
                    className="h-9 text-sm"
                    value={c.message || ''}
                    onChange={(e) => {
                      const updated = [...(form.doc_constraints || [])]
                      updated[ci] = { ...updated[ci], message: e.target.value }
                      setForm({ ...form, doc_constraints: updated })
                    }}
                    placeholder="End date must be after start date"
                  />
                </div>
              )}
            </div>
          ))}
        </section>
      </div>

      {/* YAML panel (desktop only) */}
      <div className="hidden md:block w-[42%] border-l overflow-hidden shrink-0">
        <YamlPanel form={form} onApply={(parsed) => setForm({ ...form, ...parsed })} />
      </div>
    </div>
    </div>
  )
}

function DoctypeCreationWizard({
  form,
  setForm,
  step,
  setStep,
  expandedField,
  setExpandedField,
  selectedScreens,
  setSelectedScreens,
  doctypes,
  error,
  issues,
  saving,
  onCancel,
  onCreate,
  onFieldChange,
  onFieldRemove,
  onFieldMove,
}: {
  form: DocType
  setForm: (value: DocType | ((previous: DocType) => DocType)) => void
  step: WizardStep
  setStep: (step: WizardStep) => void
  expandedField: number | null
  setExpandedField: (value: number | null) => void
  selectedScreens: StandardPageKind[]
  setSelectedScreens: (screens: StandardPageKind[]) => void
  doctypes: DocType[]
  error: string | null
  issues: DraftIssue[]
  saving: boolean
  onCancel: () => void
  onCreate: () => void
  onFieldChange: (index: number, updates: Partial<Field>) => void
  onFieldRemove: (index: number) => void
  onFieldMove: (index: number, direction: -1 | 1) => void
}) {
  const stepIndex = WIZARD_STEPS.indexOf(step)
  const progress = Math.round(((stepIndex + 1) / WIZARD_STEPS.length) * 100)
  const canContinue = !validateWizardStep(form, step)
  const stepTitle: Record<WizardStep, string> = {
    Basics: 'Name the data object',
    Fields: 'Add the fields',
    Advanced: 'Configure YAML-level settings',
    Screens: 'Pick the screens',
    Review: 'Review',
  }

  const goNext = () => {
    if (!canContinue) return
    setStep(WIZARD_STEPS[Math.min(stepIndex + 1, WIZARD_STEPS.length - 1)])
  }

  return (
    <div className="min-h-[calc(100vh-4rem)] bg-muted/20">
      <header className="border-b bg-background px-4 py-3 md:px-6">
        <div className="mx-auto flex max-w-5xl items-center justify-between gap-4">
          <div>
            <p className="text-xs font-semibold uppercase tracking-[0.18em] text-muted-foreground">Data objects</p>
            <h1 className="text-2xl font-semibold tracking-tight">Create data object</h1>
          </div>
          <Button variant="ghost" onClick={onCancel}>Cancel</Button>
        </div>
      </header>

      <main className="mx-auto grid max-w-5xl gap-4 p-4 lg:grid-cols-[190px_minmax(0,1fr)] md:p-6">
        <aside className="space-y-3">
          <Card className="shadow-none">
            <CardHeader className="space-y-3 pb-2">
              <CardTitle className="text-sm">Step {stepIndex + 1} of {WIZARD_STEPS.length}</CardTitle>
              <div className="h-1.5 rounded-full bg-muted">
                <div className="h-full rounded-full bg-foreground transition-all" style={{ width: `${progress}%` }} />
              </div>
            </CardHeader>
            <CardContent className="space-y-1.5">
              {WIZARD_STEPS.map((entry, index) => (
                <button
                  key={entry}
                  type="button"
                  className={cn(
                    'flex w-full items-center gap-3 rounded-md px-2.5 py-2 text-left text-sm transition-colors',
                    entry === step ? 'bg-foreground text-background' : 'text-muted-foreground hover:bg-muted hover:text-foreground',
                    index > stepIndex + 1 && 'opacity-50',
                  )}
                  disabled={index > stepIndex + 1}
                  onClick={() => setStep(entry)}
                >
                  <span className={cn(
                    'flex h-5 w-5 items-center justify-center rounded-full border text-[11px] font-semibold',
                    entry === step ? 'border-background/40 bg-background text-foreground' : index < stepIndex ? 'border-foreground bg-foreground text-background' : 'bg-background',
                  )}>
                    {index + 1}
                  </span>
                  <span>{entry}</span>
                </button>
              ))}
            </CardContent>
          </Card>
        </aside>

        <section className="space-y-4">
          {(error || issues.length > 0) && (
            <div className="space-y-2 rounded-lg border border-amber-500/30 bg-amber-50 p-3 text-sm text-amber-950">
              {error && <p className="text-destructive">{error}</p>}
              {issues.length > 0 && (
                <ul className="space-y-1">
                  {issues.map((issue, index) => (
                    <li key={`${issue.field ?? 'general'}-${index}`}>- {issue.message}</li>
                  ))}
                </ul>
              )}
            </div>
          )}

          <div className="flex flex-wrap items-center justify-between gap-3">
            <div>
              <h2 className="text-lg font-semibold tracking-tight">{stepTitle[step]}</h2>
              <p className="text-sm text-muted-foreground">{form.name || 'Untitled object'}</p>
            </div>
            <div className="flex flex-wrap gap-2">
              <Badge variant="outline">{form.fields.filter((field) => field.fieldname).length} fields</Badge>
              <Badge variant="outline">{selectedScreens.length} screens</Badge>
            </div>
          </div>

          <WizardAssistantPanel
            form={form}
            setForm={setForm}
            setSelectedScreens={setSelectedScreens}
            setStep={setStep}
          />

          {step === 'Basics' && (
            <Card className="shadow-none">
              <CardHeader className="pb-2">
                <CardTitle className="flex items-center gap-2">
                  What are you tracking?
                  <HelpTooltip label="Basics help">Use business words. Kigae handles the safe internal doctype name.</HelpTooltip>
                </CardTitle>
              </CardHeader>
              <CardContent className="grid gap-3 md:grid-cols-2">
                <div className="space-y-1.5">
                  <FieldLabelWithHelp label="Name" help="Choose a short name users will recognize in navigation." />
                  <Input
                    value={form.name}
                    onChange={(event) => setForm((previous) => ({
                      ...previous,
                      name: titleCase(event.target.value),
                    }))}
                    placeholder="Workspace Record"
                    autoFocus
                  />
                </div>
                <div className="space-y-1.5">
                  <FieldLabelWithHelp label="Area" help="Groups this object with related screens in the workspace." />
                  <Input
                    value={form.module}
                    onChange={(event) => setForm((previous) => ({ ...previous, module: titleCase(event.target.value) }))}
                    placeholder="Workspace"
                  />
                </div>
                <div className="space-y-1.5">
                  <FieldLabelWithHelp label="Resource name" help="Optional. Use this if the API/resource name should differ from the display name." />
                  <Input
                    value={form.resource_name || ''}
                    onChange={(event) => setForm((previous) => ({ ...previous, resource_name: event.target.value }))}
                    placeholder="customer_visit"
                  />
                </div>
                <div className="space-y-1.5 md:col-span-2">
                  <FieldLabelWithHelp label="Description" help="Optional. Keep it short; it is for admin context, not customer-facing copy." />
                  <Input
                    value={form.description}
                    onChange={(event) => setForm((previous) => ({ ...previous, description: event.target.value }))}
                    placeholder="Tracks visits, outcomes, and next actions."
                  />
                </div>
                <div className="space-y-1.5">
                  <FieldLabelWithHelp label="Title field" help="The field used as the main label in records and references." />
                  <Input
                    value={form.title_field}
                    onChange={(event) => setForm((previous) => ({ ...previous, title_field: event.target.value }))}
                    placeholder="title"
                  />
                </div>
                <div className="space-y-1.5">
                  <FieldLabelWithHelp label="Search fields" help="Comma-separated field names to include in quick search." />
                  <Input
                    value={form.search_fields}
                    onChange={(event) => setForm((previous) => ({ ...previous, search_fields: event.target.value }))}
                    placeholder="title, status"
                  />
                </div>
                <div className="space-y-1.5">
                  <FieldLabelWithHelp label="Sort field" help="Default field used when records are listed." />
                  <Input
                    value={form.sort_field}
                    onChange={(event) => setForm((previous) => ({ ...previous, sort_field: event.target.value }))}
                    placeholder="modified"
                  />
                </div>
                <div className="space-y-1.5">
                  <FieldLabelWithHelp label="Sort order" help="Default order used when records are listed." />
                  <Select value={form.sort_order} onValueChange={(value) => setForm((previous) => ({ ...previous, sort_order: value || 'DESC' }))}>
                    <SelectTrigger>
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectItem value="ASC">ASC</SelectItem>
                      <SelectItem value="DESC">DESC</SelectItem>
                    </SelectContent>
                  </Select>
                </div>
                <label className="flex items-center gap-2 rounded-lg border bg-muted/20 p-3 text-sm">
                  <Switch checked={form.track_changes} onCheckedChange={(value) => setForm((previous) => ({ ...previous, track_changes: value }))} />
                  <span className="flex items-center gap-1.5">
                    Audit trail
                    <HelpTooltip label="Audit trail help">Records who changed this data and when.</HelpTooltip>
                  </span>
                </label>
                <label className="flex items-center gap-2 rounded-lg border bg-muted/20 p-3 text-sm">
                  <Switch checked={form.is_submittable} onCheckedChange={(value) => setForm((previous) => ({ ...previous, is_submittable: value }))} />
                  <span className="flex items-center gap-1.5">
                    Approval step
                    <HelpTooltip label="Approval step help">Use when records must be submitted before they are final.</HelpTooltip>
                  </span>
                </label>
                <label className="flex items-center gap-2 rounded-lg border bg-muted/20 p-3 text-sm">
                  <Switch checked={form.is_child_table} onCheckedChange={(value) => setForm((previous) => ({ ...previous, is_child_table: value }))} />
                  <span className="flex items-center gap-1.5">
                    Child table
                    <HelpTooltip label="Child table help">Use when this doctype only lives inside another record.</HelpTooltip>
                  </span>
                </label>
                <label className="flex items-center gap-2 rounded-lg border bg-muted/20 p-3 text-sm">
                  <Switch checked={form.is_single} onCheckedChange={(value) => setForm((previous) => ({ ...previous, is_single: value }))} />
                  <span className="flex items-center gap-1.5">
                    Single record
                    <HelpTooltip label="Single record help">Use when there should only ever be one document of this type.</HelpTooltip>
                  </span>
                </label>
              </CardContent>
            </Card>
          )}

          {step === 'Fields' && (
            <Card className="shadow-none">
              <CardHeader className="pb-2">
                <CardTitle className="flex items-center gap-2">
                  What information should it collect?
                  <HelpTooltip label="Fields help">Use the full field editor. The wizard keeps attachments, computed fields, dependencies, and other advanced settings available.</HelpTooltip>
                </CardTitle>
              </CardHeader>
              <CardContent className="space-y-3">
                <div className="grid gap-2 md:grid-cols-3">
                  {STARTER_TEMPLATES.map((template) => (
                    <button
                      key={template.label}
                      type="button"
                      className="rounded-lg border bg-muted/20 p-3 text-left transition-colors hover:border-foreground hover:bg-muted/40"
                      onClick={() => setForm((previous) => applyStarterTemplate(previous, template.fields))}
                    >
                      <p className="flex items-center gap-1.5 text-sm font-medium">
                        {template.label}
                        <HelpTooltip label={`${template.label} template help`}>{template.description}</HelpTooltip>
                      </p>
                    </button>
                  ))}
                </div>
                <div className="space-y-2">
                  {form.fields.map((field, index) => (
                    <FieldRow
                      key={index}
                      field={field}
                      index={index}
                      expanded={expandedField === index}
                      onToggle={() => setExpandedField(expandedField === index ? null : index)}
                      onChange={(patch) => onFieldChange(index, patch)}
                      onRemove={() => onFieldRemove(index)}
                      onMoveUp={() => onFieldMove(index, -1)}
                      onMoveDown={() => onFieldMove(index, 1)}
                      canMoveUp={index > 0}
                      canMoveDown={index < form.fields.length - 1}
                      allDoctypes={doctypes.map((doctype) => doctype.name)}
                    />
                  ))}
                </div>
                <Button variant="outline" onClick={() => setForm((previous) => ({
                  ...previous,
                  fields: [...previous.fields, createWizardField('New field')],
                }))}>
                  <Plus className="mr-2 h-4 w-4" />
                  Add field
                </Button>
              </CardContent>
            </Card>
          )}

          {step === 'Advanced' && (
            <div className="space-y-4">
              <DocConstraintsEditor form={form} setForm={setForm} />
              <PublicAccessEditor form={form} setForm={setForm} />
            </div>
          )}

          {step === 'Screens' && (
            <Card className="shadow-none">
              <CardHeader className="pb-2">
                <CardTitle className="flex items-center gap-2">
                  Which screens should Kigae create?
                  <HelpTooltip label="Screens help">These are generated page manifests connected to this doctype.</HelpTooltip>
                </CardTitle>
              </CardHeader>
              <CardContent className="grid gap-2 md:grid-cols-2">
                {STANDARD_PAGE_KINDS.map((screen) => {
                  const selected = selectedScreens.includes(screen.kind)
                  return (
                    <button
                      key={screen.kind}
                      type="button"
                      className={cn(
                        'rounded-lg border p-4 text-left transition-colors',
                        selected ? 'border-foreground bg-foreground text-background' : 'bg-card hover:bg-muted/40',
                      )}
                      onClick={() => setSelectedScreens(toggleScreen(selectedScreens, screen.kind))}
                    >
                      <div className="flex items-start justify-between gap-3">
                        <div>
                          <p className="flex items-center gap-1.5 font-medium">
                            {screen.label}
                            <HelpTooltip label={`${screen.label} help`}>{screen.description}</HelpTooltip>
                          </p>
                        </div>
                        <Badge variant={selected ? 'secondary' : 'outline'}>{selected ? 'Included' : 'Optional'}</Badge>
                      </div>
                    </button>
                  )
                })}
              </CardContent>
            </Card>
          )}

          {step === 'Review' && (
            <Card className="shadow-none">
              <CardHeader className="pb-2">
                <CardTitle>Review and create</CardTitle>
              </CardHeader>
              <CardContent className="space-y-4">
                <div className="grid gap-3 md:grid-cols-3">
                  <SummaryCard label="Data object" value={form.name || 'Missing'} />
                  <SummaryCard label="Fields" value={`${form.fields.filter((field) => field.fieldname).length}`} />
                  <SummaryCard label="Screens" value={`${selectedScreens.length}`} />
                </div>
                <div className="rounded-xl border bg-muted/20 p-4">
                  <p className="text-sm font-medium">Created screens</p>
                  <div className="mt-2 flex flex-wrap gap-2">
                    {selectedScreens.length ? selectedScreens.map((screen) => (
                      <Badge key={screen} variant="secondary">{screen}</Badge>
                    )) : <span className="text-sm text-muted-foreground">No screens selected.</span>}
                  </div>
                </div>
              </CardContent>
            </Card>
          )}

          <div className="flex items-center justify-between rounded-lg border bg-background p-3">
            <Button variant="ghost" disabled={stepIndex === 0 || saving} onClick={() => setStep(WIZARD_STEPS[Math.max(stepIndex - 1, 0)])}>
              Back
            </Button>
            <div className="flex gap-2">
              {step !== 'Review' ? (
                <Button disabled={!canContinue || saving} onClick={goNext}>Continue</Button>
              ) : (
                <Button disabled={issues.length > 0 || saving} onClick={onCreate}>
                  {saving ? 'Creating...' : 'Create data object and screens'}
                </Button>
              )}
            </div>
          </div>
        </section>
      </main>
    </div>
  )
}

function SimpleFieldRow({
  field,
  index,
  canRemove,
  allDoctypes,
  onChange,
  onRemove,
}: {
  field: Field
  index: number
  canRemove: boolean
  allDoctypes: string[]
  onChange: (patch: Partial<Field>) => void
  onRemove: () => void
}) {
  return (
    <div className="grid gap-2 rounded-lg border bg-card p-3 md:grid-cols-[1.1fr_160px_1fr_auto]">
      <div className="space-y-1.5">
        <Label className="text-xs">Field label</Label>
        <Input
          value={field.label}
          onChange={(event) => {
            const label = event.target.value
            onChange({ label, fieldname: slugField(label) })
          }}
          placeholder={index === 0 ? 'Title' : 'Amount'}
        />
      </div>
      <div className="space-y-1.5">
        <Label className="text-xs">Type</Label>
        <Select value={field.fieldtype} onValueChange={(value) => onChange({ fieldtype: value as Field['fieldtype'], options: value === 'Select' ? field.options || 'Open\nClosed' : field.options })}>
          <SelectTrigger><SelectValue /></SelectTrigger>
          <SelectContent>
            {['Data', 'Text', 'Int', 'Currency', 'Check', 'Date', 'Datetime', 'Select', 'Link', 'Attach', 'Attach Image', 'Attach Audio'].map((type) => (
              <SelectItem key={type} value={type}>{friendlyFieldType(type)}</SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>
      <div className="space-y-1.5">
        <Label className="text-xs">{field.fieldtype === 'Link' ? 'Links to' : field.fieldtype === 'Select' ? 'Choices' : 'Field key'}</Label>
        {field.fieldtype === 'Link' ? (
          <Select value={field.options} onValueChange={(value) => onChange({ options: value || '' })}>
            <SelectTrigger><SelectValue placeholder="Choose data object" /></SelectTrigger>
            <SelectContent>{allDoctypes.map((name) => <SelectItem key={name} value={name}>{name}</SelectItem>)}</SelectContent>
          </Select>
        ) : (
          <Input
            value={field.fieldtype === 'Select' ? field.options : field.fieldname}
            onChange={(event) => onChange(field.fieldtype === 'Select' ? { options: event.target.value } : { fieldname: slugField(event.target.value) })}
            placeholder={field.fieldtype === 'Select' ? 'Open, Closed' : 'field_key'}
          />
        )}
      </div>
      <div className="flex items-end gap-2">
        <label className="mb-2 flex items-center gap-1.5 text-xs">
          <Switch checked={field.reqd} onCheckedChange={(value) => onChange({ reqd: value })} />
          Required
        </label>
        {canRemove && (
          <Button variant="ghost" size="icon" className="mb-1 text-destructive" onClick={onRemove}>
            <Trash2 className="h-4 w-4" />
          </Button>
        )}
      </div>
    </div>
  )
}

function SummaryCard({ label, value }: { label: string; value: string }) {
  return (
    <div className="rounded-lg border bg-muted/20 p-4">
      <p className="text-xs font-medium uppercase tracking-wide text-muted-foreground">{label}</p>
      <p className="mt-2 text-lg font-semibold">{value}</p>
    </div>
  )
}

function WizardAssistantPanel({
  form,
  setForm,
  setSelectedScreens,
  setStep,
}: {
  form: DocType
  setForm: (value: DocType | ((previous: DocType) => DocType)) => void
  setSelectedScreens: (screens: StandardPageKind[]) => void
  setStep: (step: WizardStep) => void
}) {
  const [prompt, setPrompt] = useState('')
  const [material, setMaterial] = useState('')
  const [materialNote, setMaterialNote] = useState<string | null>(null)
  const [draft, setDraft] = useState<AssistantDraft | null>(null)
  const [expanded, setExpanded] = useState(false)
  const [showImport, setShowImport] = useState(false)

  const generateDraft = () => {
    const next = material.trim() ? inferMaterialDraft(material, form) : buildGenericEditorDraft(form.name)
    setDraft(next)
  }

  const handleMaterialFile = async (file: File | undefined) => {
    if (!file) return
    if (file.type.startsWith('image/')) {
      setMaterialNote('Image received. OCR extraction needs the typed backend AI endpoint; paste visible labels for now.')
      return
    }
    const text = await file.text()
    setMaterial(text.slice(0, 12_000))
    setMaterialNote(`Loaded ${file.name}. Review the extracted draft before applying.`)
  }

  const applyDraft = () => {
    if (!draft) return
    setForm((previous) => {
      const base = previous.name.trim() ? previous : {
        ...previous,
        name: genericEditorDefaults().name,
        module: previous.module || genericEditorDefaults().module,
      }
      return applyStarterTemplate(base, draft.fields)
    })
    setSelectedScreens(draft.screens)
    setStep('Fields')
  }

  return (
    <Card className="shadow-none">
      <CardHeader className="space-y-0 p-3">
        <div className="flex items-center gap-2">
          <button
            type="button"
            className="flex flex-1 items-center justify-between gap-3 rounded-md text-left"
            aria-expanded={expanded}
            onClick={() => setExpanded((value) => !value)}
          >
            <CardTitle className="text-sm">AI automation support</CardTitle>
            <ChevronDown className={cn('h-4 w-4 shrink-0 text-muted-foreground transition-transform', expanded && 'rotate-180')} />
          </button>
          <HelpTooltip label="AI automation support help">AI tool used to support automation setup from workflow notes, CSVs, or existing material.</HelpTooltip>
        </div>
      </CardHeader>
      {expanded && (
        <CardContent className="space-y-3 pt-0">
          <Textarea
            value={prompt}
            onChange={(event) => setPrompt(event.target.value)}
            placeholder="Describe the automation or workflow..."
            className="min-h-16 text-xs"
          />
          <div className="flex flex-wrap items-center gap-2">
            <Button type="button" size="sm" variant="outline" disabled={!prompt.trim() && !material.trim()} onClick={generateDraft}>
              Suggest
            </Button>
            <Button type="button" size="sm" disabled={!draft} onClick={applyDraft}>
              Apply
            </Button>
            <Button type="button" size="sm" variant="ghost" onClick={() => setShowImport((value) => !value)}>
              {showImport ? 'Hide import' : 'Import file'}
            </Button>
          </div>
          {showImport && (
            <div className="rounded-lg border bg-muted/20 p-2">
              <FieldLabelWithHelp label="Material" help="Paste CSV headers, a checklist, or notes. Text files are read locally before suggestions are generated." className="text-xs" />
              <Textarea
                value={material}
                onChange={(event) => setMaterial(event.target.value)}
                placeholder="customer,status,next_action,due_date"
                className="mt-1 min-h-16 text-xs"
              />
              <Input
                type="file"
                accept=".csv,.txt,.md,text/csv,text/plain,image/*"
                className="mt-2 h-8 text-xs"
                onChange={(event) => void handleMaterialFile(event.target.files?.[0])}
              />
              {materialNote && <p className="mt-1 text-xs text-muted-foreground">{materialNote}</p>}
            </div>
          )}
          {draft && (
            <div className="space-y-2 rounded-lg border bg-muted/20 p-3 text-xs">
              <p className="font-medium">{draft.reply}</p>
              <div className="flex flex-wrap gap-1">
                {draft.fields.map(([label, type]) => (
                  <Badge key={`${label}:${type}`} variant="secondary">{label}</Badge>
                ))}
              </div>
              <p className="text-muted-foreground">Screens: {draft.screens.join(', ')}</p>
            </div>
          )}
        </CardContent>
      )}
    </Card>
  )
}

function DocConstraintsEditor({
  form,
  setForm,
}: {
  form: DocType
  setForm: (value: DocType | ((previous: DocType) => DocType)) => void
}) {
  const constraints = form.doc_constraints || []

  const updateConstraint = (index: number, updates: Partial<DocConstraint>) => {
    setForm((previous) => {
      const next = [...(previous.doc_constraints || [])]
      next[index] = { ...next[index], ...updates }
      return { ...previous, doc_constraints: next }
    })
  }

  const addConstraint = () => {
    setForm((previous) => ({
      ...previous,
      doc_constraints: [...(previous.doc_constraints || []), { ...EMPTY_DOC_CONSTRAINT }],
    }))
  }

  const removeConstraint = (index: number) => {
    setForm((previous) => ({
      ...previous,
      doc_constraints: (previous.doc_constraints || []).filter((_, i) => i !== index),
    }))
  }

  return (
    <Card className="shadow-none">
      <CardHeader className="pb-2">
        <CardTitle className="flex items-center justify-between gap-3">
          <span>Document constraints</span>
          <Button variant="outline" size="sm" onClick={addConstraint}>
            <Plus className="mr-2 h-4 w-4" />
            Add rule
          </Button>
        </CardTitle>
        <CardDescription>Expose the full doc_constraints YAML surface here.</CardDescription>
      </CardHeader>
      <CardContent className="space-y-3">
        {constraints.length === 0 && (
          <p className="text-sm text-muted-foreground">No document constraints yet.</p>
        )}
        {constraints.map((constraint, index) => (
          <div key={index} className="rounded-lg border p-3 space-y-3">
            <div className="grid gap-3 md:grid-cols-3">
              <div className="space-y-1.5">
                <Label>Type</Label>
                <Select value={constraint.type || ''} onValueChange={(value) => updateConstraint(index, { type: value || 'Predicate' })}>
                  <SelectTrigger>
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {['Predicate', 'field_dependency', 'cross_field', 'immutable_after'].map((type) => (
                      <SelectItem key={type} value={type}>{type}</SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>
              <div className="space-y-1.5">
                <Label>Description</Label>
                <Input value={constraint.description || ''} onChange={(event) => updateConstraint(index, { description: event.target.value })} />
              </div>
              <div className="space-y-1.5">
                <Label>Message</Label>
                <Input value={constraint.message} onChange={(event) => updateConstraint(index, { message: event.target.value })} />
              </div>
            </div>

            <div className="grid gap-3 md:grid-cols-3">
              <div className="space-y-1.5 md:col-span-2">
                <Label>Predicate</Label>
                <LispAutocomplete
                  value={constraint.predicate || ''}
                  onChange={(value) => updateConstraint(index, { predicate: value })}
                  placeholder="(> end_date start_date)"
                  fieldNames={form.fields.map((field) => field.fieldname)}
                />
              </div>
              <div className="space-y-1.5">
                <Label>Condition</Label>
                <Input value={constraint.condition || ''} onChange={(event) => updateConstraint(index, { condition: event.target.value })} />
              </div>
            </div>

            <div className="grid gap-3 md:grid-cols-3">
              <div className="space-y-1.5">
                <Label>Require fields</Label>
                <Textarea value={joinList(constraint.require_fields)} onChange={(event) => updateConstraint(index, { require_fields: splitList(event.target.value) })} className="min-h-20" />
              </div>
              <div className="space-y-1.5">
                <Label>Field</Label>
                <Input value={constraint.field || ''} onChange={(event) => updateConstraint(index, { field: event.target.value })} />
              </div>
              <div className="space-y-1.5">
                <Label>Group by</Label>
                <Textarea value={joinList(constraint.group_by)} onChange={(event) => updateConstraint(index, { group_by: splitList(event.target.value) })} className="min-h-20" />
              </div>
            </div>

            <div className="grid gap-3 md:grid-cols-3">
              <div className="space-y-1.5">
                <Label>LHS</Label>
                <Input value={constraint.lhs || ''} onChange={(event) => updateConstraint(index, { lhs: event.target.value })} />
              </div>
              <div className="space-y-1.5">
                <Label>Operator</Label>
                <Input value={constraint.operator || ''} onChange={(event) => updateConstraint(index, { operator: event.target.value })} />
              </div>
              <div className="space-y-1.5">
                <Label>RHS</Label>
                <Input value={constraint.rhs || ''} onChange={(event) => updateConstraint(index, { rhs: event.target.value })} />
              </div>
            </div>

            <div className="grid gap-3 md:grid-cols-3">
              <div className="space-y-1.5">
                <Label>Fields</Label>
                <Textarea value={joinList(constraint.fields)} onChange={(event) => updateConstraint(index, { fields: splitList(event.target.value) })} className="min-h-20" />
              </div>
              <div className="space-y-1.5">
                <Label>Status field</Label>
                <Input value={constraint.status_field || ''} onChange={(event) => updateConstraint(index, { status_field: event.target.value })} />
              </div>
              <div className="space-y-1.5">
                <Label>Status values</Label>
                <Textarea value={joinList(constraint.status_values)} onChange={(event) => updateConstraint(index, { status_values: splitList(event.target.value) })} className="min-h-20" />
              </div>
            </div>

            <div className="grid gap-3 md:grid-cols-3">
              <div className="space-y-1.5">
                <Label>Immutable fields</Label>
                <Textarea value={joinList(constraint.immutable_fields)} onChange={(event) => updateConstraint(index, { immutable_fields: splitList(event.target.value) })} className="min-h-20" />
              </div>
              <div className="space-y-1.5">
                <Label>Value</Label>
                <Input value={constraint.value != null ? String(constraint.value) : ''} onChange={(event) => updateConstraint(index, { value: parseConstraintValue(event.target.value) })} />
              </div>
              <div className="space-y-1.5">
                <Label>Nested constraints</Label>
                <ConstraintListEditor
                  constraints={constraint.constraints || []}
                  onChange={(next) => updateConstraint(index, { constraints: next })}
                />
              </div>
            </div>

            <div className="flex justify-end">
              <Button variant="ghost" size="sm" className="text-destructive" onClick={() => removeConstraint(index)}>Remove rule</Button>
            </div>
          </div>
        ))}
      </CardContent>
    </Card>
  )
}

function PublicAccessEditor({
  form,
  setForm,
}: {
  form: DocType
  setForm: (value: DocType | ((previous: DocType) => DocType)) => void
}) {
  const pa = form.public_access || { ...EMPTY_PUBLIC_ACCESS }
  const updateAccess = (updates: Partial<PublicAccess>) => {
    setForm((previous) => ({
      ...previous,
      public_access: {
        ...(previous.public_access || { ...EMPTY_PUBLIC_ACCESS }),
        ...updates,
      },
    }))
  }
  const updatePublicRecords = (enabled: boolean) => {
    setForm((previous) => {
      const next = ensurePublicVisibilityConvention(previous, enabled)
      return next
    })
  }
  const updateFilter = (index: number, updates: Partial<PublicFilter>) => {
    setForm((previous) => {
      const current = previous.public_access || { ...EMPTY_PUBLIC_ACCESS }
      const nextFilters = [...(current.filters || [])]
      nextFilters[index] = { ...nextFilters[index], ...updates }
      return {
        ...previous,
        public_access: {
          ...current,
          filters: nextFilters,
        },
      }
    })
  }

  return (
    <Card className="shadow-none">
      <CardHeader className="pb-2">
        <CardTitle className="flex items-center justify-between gap-3">
          <span>Public records</span>
          <label className="flex items-center gap-2 text-sm font-normal">
            <Switch checked={pa.enabled} onCheckedChange={updatePublicRecords} />
            Enable convention
          </label>
        </CardTitle>
        <CardDescription>Expose public records through `is_public` and keep the full `public_access` YAML surface available.</CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        <div className="rounded-lg border bg-muted/20 p-3 text-sm">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <div>
              <p className="font-medium">Public/private visibility convention</p>
              <p className="text-muted-foreground">
                Add an `is_public` checkbox to the doctype and use it to gate public API access.
              </p>
            </div>
          </div>
          <p className="mt-2 text-xs text-muted-foreground">
            Public rows are filtered with `is_public = true`. New records stay private until checked.
          </p>
        </div>
        <div className="grid gap-3 md:grid-cols-2">
          <label className="flex items-center gap-2 rounded-lg border bg-muted/20 p-3 text-sm">
            <Switch checked={pa.list} onCheckedChange={(value) => updateAccess({ list: value })} />
            <span>Allow list</span>
          </label>
          <label className="flex items-center gap-2 rounded-lg border bg-muted/20 p-3 text-sm">
            <Switch checked={pa.read} onCheckedChange={(value) => updateAccess({ read: value })} />
            <span>Allow read</span>
          </label>
        </div>
        <div className="grid gap-3 md:grid-cols-2">
          <div className="space-y-1.5">
            <Label>Fields</Label>
            <Textarea value={joinList(pa.fields)} onChange={(event) => updateAccess({ fields: splitList(event.target.value) })} className="min-h-20" />
          </div>
          <div className="space-y-1.5">
            <Label>Sort field</Label>
            <Input value={pa.sort_field} onChange={(event) => updateAccess({ sort_field: event.target.value })} />
            <Label className="mt-3 block">Sort order</Label>
            <Select value={pa.sort_order} onValueChange={(value) => updateAccess({ sort_order: value || 'DESC' })}>
              <SelectTrigger className="mt-1"><SelectValue /></SelectTrigger>
              <SelectContent>
                <SelectItem value="ASC">ASC</SelectItem>
                <SelectItem value="DESC">DESC</SelectItem>
              </SelectContent>
            </Select>
          </div>
        </div>
        <div className="grid gap-3 md:grid-cols-2">
          <div className="space-y-1.5">
            <Label>Max limit</Label>
            <Input value={String(pa.max_limit ?? '')} onChange={(event) => updateAccess({ max_limit: toNumberOrZero(event.target.value) })} />
          </div>
          <div className="space-y-1.5">
            <Label>Cache max age</Label>
            <Input value={String(pa.cache_max_age ?? '')} onChange={(event) => updateAccess({ cache_max_age: toNumberOrZero(event.target.value) })} />
          </div>
        </div>
        <div className="space-y-3">
          <div className="flex items-center justify-between">
            <Label>Filters</Label>
            <Button variant="outline" size="sm" onClick={() => updateAccess({ filters: [...pa.filters, { field: '', op: 'equals', value: '' }] })}>
              <Plus className="mr-2 h-4 w-4" />
              Add filter
            </Button>
          </div>
          {pa.enabled && pa.filters.length === 1 && pa.filters[0].field === PUBLIC_VISIBILITY_FIELDNAME && (
            <p className="text-xs text-muted-foreground">
              The visibility filter is locked to `is_public = true` while public records are enabled.
            </p>
          )}
          {(pa.filters || []).map((filter, index) => (
            <div key={index} className="grid gap-2 md:grid-cols-[1fr_1fr_1fr_auto]">
              <Input
                value={filter.field}
                onChange={(event) => updateFilter(index, { field: event.target.value })}
                placeholder="status"
                disabled={pa.enabled && filter.field === PUBLIC_VISIBILITY_FIELDNAME}
              />
              <Select
                value={filter.op || ''}
                onValueChange={(value) => updateFilter(index, { op: value || 'equals' })}
                disabled={pa.enabled && filter.field === PUBLIC_VISIBILITY_FIELDNAME}
              >
                <SelectTrigger><SelectValue /></SelectTrigger>
                <SelectContent>
                  <SelectItem value="equals">equals</SelectItem>
                  <SelectItem value="not_equals">not_equals</SelectItem>
                  <SelectItem value="in">in</SelectItem>
                  <SelectItem value="is_set">is_set</SelectItem>
                  <SelectItem value="is_not_set">is_not_set</SelectItem>
                </SelectContent>
              </Select>
              <Input
                value={String(filter.value ?? '')}
                onChange={(event) => updateFilter(index, { value: event.target.value })}
                placeholder="published"
                disabled={pa.enabled && filter.field === PUBLIC_VISIBILITY_FIELDNAME}
              />
              <Button
                variant="ghost"
                size="icon"
                className="text-destructive"
                onClick={() => updateAccess({ filters: pa.filters.filter((_, i) => i !== index) })}
                disabled={pa.enabled && filter.field === PUBLIC_VISIBILITY_FIELDNAME}
              >
                <Trash2 className="h-4 w-4" />
              </Button>
            </div>
          ))}
        </div>
      </CardContent>
    </Card>
  )
}

function ConstraintListEditor({
  constraints,
  onChange,
  addLabel = 'Add nested constraint',
  emptyLabel = 'No nested constraints.',
}: {
  constraints: Constraint[]
  onChange: (constraints: Constraint[]) => void
  addLabel?: string
  emptyLabel?: string
}) {
  const updateConstraint = (index: number, updates: Partial<Constraint>) => {
    const next = [...constraints]
    next[index] = { ...next[index], ...updates }
    onChange(next)
  }

  return (
    <div className="space-y-3">
      {(constraints || []).length === 0 && <p className="text-xs text-muted-foreground">{emptyLabel}</p>}
      {(constraints || []).map((constraint, index) => (
        <div key={index} className="rounded-md border p-2 space-y-2">
          <div className="grid gap-2 md:grid-cols-2">
            <Select value={constraint.type || ''} onValueChange={(value) => updateConstraint(index, { type: value || 'max' })}>
              <SelectTrigger><SelectValue /></SelectTrigger>
              <SelectContent>
                {['max', 'min', 'max_length', 'min_length', 'max_rows', 'min_rows', 'regex', 'one_of', 'not_one_of', 'min_date', 'max_date', 'exists', 'unique_in', 'required_if'].map((type) => (
                  <SelectItem key={type} value={type}>{type}</SelectItem>
                ))}
              </SelectContent>
            </Select>
            <Input value={constraint.message} onChange={(event) => updateConstraint(index, { message: event.target.value })} placeholder="message" />
          </div>
          <ConstraintFieldsEditor constraint={constraint} onChange={(updates) => updateConstraint(index, updates)} />
          <div className="flex justify-end">
            <Button variant="ghost" size="sm" className="text-destructive" onClick={() => onChange(constraints.filter((_, i) => i !== index))}>Remove</Button>
          </div>
        </div>
      ))}
      <Button variant="outline" size="sm" onClick={() => onChange([...(constraints || []), { type: 'max', message: '' }])}>
        <Plus className="mr-2 h-4 w-4" />
        {addLabel}
      </Button>
    </div>
  )
}

function ConstraintFieldsEditor({
  constraint,
  onChange,
}: {
  constraint: Constraint
  onChange: (updates: Partial<Constraint>) => void
}) {
  return (
    <div className="grid gap-2 md:grid-cols-3">
      <Input value={constraint.value != null ? String(constraint.value) : ''} onChange={(event) => onChange({ value: parseConstraintValue(event.target.value) })} placeholder="value" />
      <Input value={joinList(constraint.values)} onChange={(event) => onChange({ values: splitList(event.target.value) })} placeholder="values" />
      <Input value={constraint.pattern || ''} onChange={(event) => onChange({ pattern: event.target.value })} placeholder="pattern" />
      <Input value={constraint.condition || ''} onChange={(event) => onChange({ condition: event.target.value })} placeholder="condition" />
      <Input value={constraint.scope || ''} onChange={(event) => onChange({ scope: event.target.value })} placeholder="scope" />
    </div>
  )
}

function joinList(values?: string[]): string {
  return (values || []).join('\n')
}

function splitList(value: string): string[] {
  return value
    .split(/[\n,]/)
    .map((entry) => entry.trim())
    .filter(Boolean)
}

function parseConstraintValue(value: string): any {
  const trimmed = value.trim()
  if (trimmed === '') return undefined
  const numeric = Number(trimmed)
  if (!Number.isNaN(numeric) && trimmed === String(numeric)) return numeric
  if (trimmed === 'true') return true
  if (trimmed === 'false') return false
  return value
}

function toNumberOrZero(value: string): number {
  const numeric = Number(value)
  return Number.isFinite(numeric) ? numeric : 0
}

function validateWizardStep(form: DocType, step: WizardStep): string | null {
  if (step === 'Basics') {
    if (!form.name.trim()) return 'Name is required.'
    if (!form.module.trim()) return 'Area is required.'
  }
  if (step === 'Fields') {
    const fields = form.fields
      .map((field, index) => slugField(field.fieldname || field.label || `field_${index + 1}`))
      .filter(Boolean)
    if (fields.length === 0) return 'Add at least one field.'
    if (new Set(fields).size !== fields.length) return 'Field keys must be unique after system names are reserved.'
  }
  return null
}

function formatPreviewBlockers(blocked: Array<{ message: string; change: string }>): string {
  const top = blocked.slice(0, 3).map((entry) => entry.message || entry.change)
  return top.join(' ')
}

function PreflightSummary({ preview }: { preview: Awaited<ReturnType<typeof dryRunDoctype>> }) {
  const hasWarnings = preview.warnings.length > 0
  const hasBlocked = preview.blocked.length > 0
  const tone = hasBlocked ? 'border-destructive/50 bg-destructive/10 text-destructive' : hasWarnings ? 'border-amber-500/40 bg-amber-50 text-amber-900' : 'border-green-500/40 bg-green-50 text-green-900'

  return (
    <div className={cn('rounded-lg border p-3 text-sm', tone)}>
      <p className="font-medium">
        {hasBlocked ? 'Activation needs attention' : hasWarnings ? 'Activation looks mostly good' : 'Ready to activate'}
      </p>
      <p className="mt-1 text-sm">
        {hasBlocked
          ? 'Fix the blocked items below before saving and activating.'
          : hasWarnings
            ? 'Warnings are safe to review, but they may affect existing records.'
            : 'No blocking issues were found in the current configuration.'}
      </p>
      {(hasBlocked || hasWarnings) && (
        <ul className="mt-2 space-y-1 text-sm">
          {[...preview.blocked, ...preview.warnings].slice(0, 4).map((entry) => (
            <li key={`${entry.doctype}:${entry.change}:${entry.field ?? ''}`}>- {entry.message}</li>
          ))}
        </ul>
      )}
    </div>
  )
}

function inferMaterialDraft(material: string, form: DocType): AssistantDraft {
  const fields = extractMaterialFields(material)
  return {
    reply: fields.length
      ? `I found ${fields.length} fields from your material. Review names and types before creating.`
      : 'I could not confidently extract fields, so I drafted a generic record structure.',
    fields: fields.length ? fields : [['Title', 'Data'], ['Status', 'Select'], ['Date', 'Date'], ['Notes', 'Text']],
    screens: chooseScreensForFields(fields, form),
  }
}

function extractMaterialFields(material: string): Array<[string, Field['fieldtype']]> {
  const firstMeaningfulLine = material
    .split(/\r?\n/)
    .map((line) => line.trim())
    .find((line) => line && !line.startsWith('#')) || ''
  const csvLike = firstMeaningfulLine.includes(',') || firstMeaningfulLine.includes('\t') || firstMeaningfulLine.includes(';')
  const rawNames = csvLike
    ? firstMeaningfulLine.split(/,|\t|;/)
    : material.split(/\r?\n/).flatMap((line) => line.split(/:|-/).slice(0, 1))

  return rawNames
    .map((name) => name.trim().replace(/^["']|["']$/g, ''))
    .filter((name) => name.length > 1)
    .slice(0, 14)
    .map((name) => [titleCase(name.replace(/_/g, ' ')), inferFieldType(name)])
}

function inferFieldType(name: string): Field['fieldtype'] {
  const normalized = name.toLowerCase()
  if (normalized.includes('date') || normalized.includes('deadline') || normalized.includes('due')) return 'Date'
  if (normalized.includes('time')) return 'Datetime'
  if (normalized.includes('percent') || normalized.includes('percentage') || normalized.includes('margin')) return 'Percent'
  if (normalized.includes('rate') || normalized.includes('ratio') || normalized.includes('precision') || normalized.includes('factor')) return 'Float'
  if (normalized.includes('amount') || normalized.includes('price') || normalized.includes('cost') || normalized.includes('total')) return 'Currency'
  if (normalized.includes('qty') || normalized.includes('quantity') || normalized.includes('count')) return 'Int'
  if (normalized.includes('status') || normalized.includes('stage') || normalized.includes('priority') || normalized.includes('type')) return 'Select'
  if (normalized.includes('secret') || normalized.includes('password') || normalized.includes('token')) return 'Password'
  if (normalized.includes('json') || normalized.includes('config') || normalized.includes('metadata') || normalized.includes('payload')) return 'JSON'
  if (normalized.includes('details') || normalized.includes('description') || normalized.includes('body') || normalized.includes('content') || normalized.includes('notes') || normalized.includes('remarks')) return 'Text Editor'
  if (normalized.includes('table') || normalized.includes('items') || normalized.includes('lines') || normalized.includes('rows')) return 'Table'
  if (normalized.includes('reference') || normalized.includes('related doctype') || normalized.includes('dynamic link')) return 'Dynamic Link'
  if (normalized.includes('owner') || normalized.includes('user')) return 'Link'
  if (normalized.includes('note') || normalized.includes('description') || normalized.includes('comment')) return 'Text'
  if (normalized.includes('audio') || normalized.includes('voice') || normalized.includes('recording') || normalized.includes('sound')) return 'Attach Audio'
  if (normalized.includes('photo') || normalized.includes('image') || normalized.includes('picture') || normalized.includes('avatar') || normalized.includes('logo')) return 'Attach Image'
  if (normalized.includes('file') || normalized.includes('attachment') || normalized.includes('document') || normalized.includes('pdf')) return 'Attach'
  if (normalized.startsWith('is_') || normalized.includes('done')) return 'Check'
  return 'Data'
}

function chooseScreensForFields(fields: Array<[string, Field['fieldtype']]>, form: DocType): StandardPageKind[] {
  const hasMoney = fields.some(([, type]) => type === 'Currency')
  const hasManyFields = fields.length > 6
  if (form.is_submittable || hasMoney) return ['table', 'detail', 'form']
  if (hasManyFields) return ['table', 'form']
  return ['table', 'cards', 'form']
}

function normalizeWizardDoctype(form: DocType): DocType {
  return normalizeDoctypeForSave(form)
}

function normalizeDoctypeForSave(form: DocType): DocType {
  const prepared = ensurePublicVisibilityConvention(form, Boolean(form.public_access?.enabled))
  const fields = prepared.fields
    .map((field, index) => ({
      ...field,
      fieldname: slugField(field.fieldname || field.label || `field_${index + 1}`),
      label: field.label || titleCase(field.fieldname || `Field ${index + 1}`),
      options: field.fieldtype === 'Select' ? normalizeSelectOptions(field.options) : field.options,
    }))
    .filter((field) => field.fieldname)

  const publicAccess = prepared.public_access ? {
    ...prepared.public_access,
    enabled: Boolean(prepared.public_access.enabled),
    list: Boolean(prepared.public_access.enabled ? prepared.public_access.list ?? true : prepared.public_access.list),
    read: Boolean(prepared.public_access.enabled ? prepared.public_access.read ?? true : prepared.public_access.read),
    fields: prepared.public_access.enabled ? normalizePublicFields(prepared.public_access) : prepared.public_access.fields,
    filters: prepared.public_access.enabled ? normalizePublicFilters(prepared.public_access.filters || [], true) : prepared.public_access.filters,
  } : undefined

  return {
    ...prepared,
    resource_name: prepared.resource_name?.trim() || slugField(prepared.name),
    name: titleCase(prepared.name.trim()),
    module: titleCase(prepared.module.trim()),
    title_field: prepared.title_field.trim() || fields[0]?.fieldname || 'title',
    search_fields: prepared.search_fields.trim() || fields.slice(0, 4).map((field) => field.fieldname).join(','),
    sort_field: prepared.sort_field.trim() || 'modified',
    sort_order: prepared.sort_order || 'DESC',
    doc_constraints: prepared.doc_constraints?.filter((constraint) => constraint.type?.trim()) || [],
    public_access: publicAccess,
    fields,
  }
}

function ensurePublicVisibilityConvention(form: DocType, enabled: boolean): DocType {
  if (!enabled && !form.public_access) {
    return form
  }

  const next = {
    ...form,
    public_access: {
      ...(form.public_access || { ...EMPTY_PUBLIC_ACCESS }),
      enabled,
      list: enabled ? true : form.public_access?.list ?? false,
      read: enabled ? true : form.public_access?.read ?? false,
    },
  }

  if (!enabled) {
    return next
  }

  const hasField = next.fields.some((field) => field.fieldname === PUBLIC_VISIBILITY_FIELDNAME)
  const fields = hasField ? [...next.fields] : insertPublicVisibilityField(next.fields)
  const filters = normalizePublicFilters(
    next.public_access?.filters || [],
    true,
  )

  return {
    ...next,
    fields,
    public_access: {
      ...(next.public_access || { ...EMPTY_PUBLIC_ACCESS }),
      enabled: true,
      list: true,
      read: true,
      fields: normalizePublicFields(next.public_access),
      filters,
    },
  }
}

function insertPublicVisibilityField(fields: Field[]): Field[] {
  const publicField = createPublicVisibilityField()
  const next = [...fields]
  const statusIndex = next.findIndex((field) => field.fieldname === 'status')
  if (statusIndex >= 0) {
    next.splice(statusIndex + 1, 0, publicField)
    return next
  }
  if (next.length > 0) {
    next.splice(Math.min(1, next.length), 0, publicField)
    return next
  }
  return [publicField]
}

function createPublicVisibilityField(): Field {
  return {
    ...EMPTY_FIELD,
    fieldname: PUBLIC_VISIBILITY_FIELDNAME,
    fieldtype: 'Check',
    label: 'Public',
    default: '0',
    in_list_view: true,
    in_standard_filter: true,
    description: 'Marks records that should be visible through public access routes.',
  }
}

function normalizePublicFields(publicAccess?: PublicAccess): string[] {
  const seen = new Set<string>()
  const fields: string[] = []

  for (const fieldname of publicAccess?.fields || []) {
    const normalized = fieldname.trim()
    if (!normalized || normalized === PUBLIC_VISIBILITY_FIELDNAME) continue
    if (seen.has(normalized)) continue
    seen.add(normalized)
    fields.push(normalized)
  }

  if (fields.length === 0) {
    fields.push('name')
  }

  return fields
}

function normalizePublicFilters(filters: PublicFilter[], enabled: boolean): PublicFilter[] {
  if (!enabled) {
    return filters
  }

  const next = filters.filter((filter) => {
    const field = filter.field?.trim()
    if (!field) return false
    if (field === PUBLIC_VISIBILITY_FIELDNAME) return false
    return true
  })
  next.unshift(PUBLIC_VISIBILITY_FILTER)
  return next
}

function applyStarterTemplate(form: DocType, fields: Array<[string, Field['fieldtype']]>): DocType {
  const starterFields = fields.map(([label, fieldtype], index) => createWizardField(label, fieldtype, index < 3))
  return {
    ...form,
    fields: starterFields,
  }
}

function updateWizardField(form: DocType, index: number, patch: Partial<Field>): DocType {
  const fields = [...form.fields]
  fields[index] = { ...fields[index], ...patch }
  return { ...form, fields }
}

function createWizardField(label: string, fieldtype: Field['fieldtype'] = 'Data', visible = true): Field {
  return {
    ...EMPTY_FIELD,
    label,
    fieldname: slugField(label),
    fieldtype,
    options: fieldtype === 'Select' ? 'Open\nClosed' : '',
    in_list_view: visible,
    search_index: visible,
  }
}

function toggleScreen(current: StandardPageKind[], screen: StandardPageKind): StandardPageKind[] {
  return current.includes(screen) ? current.filter((entry) => entry !== screen) : [...current, screen]
}


function normalizeSelectOptions(value: string): string {
  return value.includes('\n') ? value : value.split(',').map((entry) => entry.trim()).filter(Boolean).join('\n')
}

function friendlyFieldType(type: string): string {
  if (type === 'Data') return 'Short text'
  if (type === 'Text') return 'Long text'
  if (type === 'Int') return 'Number'
  if (type === 'Currency') return 'Money'
  if (type === 'Check') return 'Yes / no'
  if (type === 'Datetime') return 'Date & time'
  if (type === 'Select') return 'Choices'
  if (type === 'Link') return 'Link to data'
  if (type === 'Attach') return 'File'
  if (type === 'Attach Image') return 'Image'
  if (type === 'Attach Audio') return 'Audio'
  return type
}

function FieldRow({
  field,
  index,
  expanded,
  onToggle,
  onChange,
  onRemove,
  onMoveUp,
  onMoveDown,
  canMoveUp,
  canMoveDown,
  allDoctypes,
}: {
  field: Field
  index: number
  expanded: boolean
  onToggle: () => void
  onChange: (updates: Partial<Field>) => void
  onRemove: () => void
  onMoveUp: () => void
  onMoveDown: () => void
  canMoveUp: boolean
  canMoveDown: boolean
  allDoctypes: string[]
}) {
  const isLayout = ['Section Break', 'Column Break', 'Heading'].includes(field.fieldtype)
  const typeColor = isLayout ? 'bg-purple-100 text-purple-800' : 'bg-blue-100 text-blue-800'

  return (
    <div className="border rounded-lg">
      {/* Collapsed summary */}
      <div
        className="flex items-center gap-2 px-3 py-2 cursor-pointer hover:bg-muted/30"
        onClick={onToggle}
      >
        <button
          className="text-muted-foreground hover:text-foreground"
          onClick={(e) => { e.stopPropagation(); onToggle() }}
        >
          {expanded ? <ChevronDown className="h-4 w-4" /> : <ChevronRight className="h-4 w-4" />}
        </button>
        <GripVertical className="h-4 w-4 text-muted-foreground/50 hidden sm:block" />
        <span className="text-sm font-medium min-w-0 sm:min-w-[100px] truncate">{field.fieldname || '(new)'}</span>
        <span className={`inline-flex items-center rounded-full px-2 py-0.5 text-xs font-medium ${typeColor}`}>
          {field.fieldtype}
        </span>
        {field.options && !isLayout && (
          <span className="text-xs text-muted-foreground hidden sm:inline truncate max-w-[100px]">→ {field.options}</span>
        )}
        <span className="text-xs text-muted-foreground flex-1 hidden sm:inline">{field.label}</span>
        {field.reqd && (
          <span className="hidden sm:inline-flex items-center rounded-full bg-amber-100 text-amber-800 dark:bg-amber-900/30 dark:text-amber-400 px-1.5 py-0.5 text-[10px] font-bold">
            REQD
          </span>
        )}
        {field.in_list_view && (
          <span className="hidden sm:inline-flex items-center rounded-full bg-green-100 text-green-800 dark:bg-green-900/30 dark:text-green-400 px-1.5 py-0.5 text-[10px] font-medium">
            LIST
          </span>
        )}
        <div className="ml-auto flex items-center gap-0.5 shrink-0" onClick={(e) => e.stopPropagation()}>
          <Button variant="ghost" size="icon" className="h-6 w-6 hidden sm:inline-flex" aria-label="Move field up" title="Move field up" onClick={() => onMoveUp()} disabled={!canMoveUp}>
            <ChevronRight className="h-3 w-3 rotate-[-90deg]" />
          </Button>
          <Button variant="ghost" size="icon" className="h-6 w-6 hidden sm:inline-flex" aria-label="Move field down" title="Move field down" onClick={() => onMoveDown()} disabled={!canMoveDown}>
            <ChevronRight className="h-3 w-3 rotate-90" />
          </Button>
          <Button variant="ghost" size="icon" className="h-6 w-6" aria-label="Toggle field details" title="Toggle details" onClick={onToggle}>
            <Edit className="h-3 w-3" />
          </Button>
          <Button variant="ghost" size="icon" className="h-6 w-6" aria-label="Remove field" title="Remove field" onClick={onRemove}>
            <Trash2 className="h-3 w-3 text-destructive" />
          </Button>
        </div>
      </div>

      {/* Expanded editor */}
      {expanded && (
        <div className="px-4 pb-4 border-t">
          <div className="grid grid-cols-1 sm:grid-cols-3 gap-3 mt-3">
            <div>
              <Label>Fieldname *</Label>
              <Input
                value={field.fieldname}
                onChange={(e) => onChange({ fieldname: e.target.value })}
                placeholder="field_name"
              />
            </div>
            <div>
              <Label>Label</Label>
              <Input
                value={field.label}
                onChange={(e) => onChange({ label: e.target.value })}
              />
            </div>
            <div>
              <Label>Type *</Label>
              <Select value={field.fieldtype} onValueChange={(v) => onChange({ fieldtype: v as any })}>
                <SelectTrigger>
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {FIELD_TYPE_GROUPS.map((group) => (
                    <div key={group.label}>
                      <div className="px-2 py-1 text-xs font-semibold text-muted-foreground">{group.label}</div>
                      {group.types.map((t) => (
                        <SelectItem key={t} value={t}>{t}</SelectItem>
                      ))}
                    </div>
                  ))}
                </SelectContent>
              </Select>
            </div>
          </div>

          {/* Type-specific options */}
          {field.fieldtype === 'Select' && (
            <div className="mt-3">
              <Label>Options (one per line)</Label>
              <textarea
                className="w-full mt-1 rounded-md border bg-background px-3 py-2 text-sm font-mono min-h-[80px]"
                value={field.options}
                onChange={(e) => onChange({ options: e.target.value })}
                placeholder="Option 1\nOption 2\nOption 3"
              />
            </div>
          )}
          {(field.fieldtype === 'Link' || field.fieldtype === 'Dynamic Link') && (
            <div className="mt-3">
              <Label>Target DocType</Label>
              <Select value={field.options} onValueChange={(v) => onChange({ options: v || '' })}>
                <SelectTrigger className="mt-1">
                  <SelectValue placeholder="Select target doctype..." />
                </SelectTrigger>
                <SelectContent>
                  {allDoctypes.map((name) => (
                    <SelectItem key={name} value={name}>{name}</SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
          )}
          {field.fieldtype === 'Table' && (
            <div className="mt-3">
              <Label>Child DocType Name</Label>
              <Input
                className="mt-1"
                value={field.options}
                onChange={(e) => onChange({ options: e.target.value })}
                placeholder="Related Record"
              />
            </div>
          )}
          {(field.fieldtype === 'Attach' || field.fieldtype === 'Attach Image' || field.fieldtype === 'Attach Audio') && (
            <div className="mt-3">
              <Label>Allowed files</Label>
              <Input
                className="mt-1"
                value={field.accept || ''}
                onChange={(e) => onChange({ accept: e.target.value })}
                placeholder=".pdf,.docx or image/*"
              />
            </div>
          )}

          {/* Display options */}
          <div className="grid grid-cols-2 sm:flex sm:flex-wrap gap-2 sm:gap-4 mt-4">
            <label className="flex items-center gap-1.5 text-sm">
              <Switch checked={field.reqd} onCheckedChange={(v) => onChange({ reqd: v })} /> Required
            </label>
            <label className="flex items-center gap-1.5 text-sm">
              <Switch checked={field.unique} onCheckedChange={(v) => onChange({ unique: v })} /> Unique
            </label>
            <label className="flex items-center gap-1.5 text-sm">
              <Switch checked={field.read_only} onCheckedChange={(v) => onChange({ read_only: v })} /> Read Only
            </label>
            <label className="flex items-center gap-1.5 text-sm">
              <Switch checked={field.bold} onCheckedChange={(v) => onChange({ bold: v })} /> Bold
            </label>
            <label className="flex items-center gap-1.5 text-sm">
              <Switch checked={field.hidden} onCheckedChange={(v) => onChange({ hidden: v })} /> Hidden
            </label>
            <label className="flex items-center gap-1.5 text-sm">
              <Switch checked={field.in_list_view} onCheckedChange={(v) => onChange({ in_list_view: v })} /> In List View
            </label>
            <label className="flex items-center gap-1.5 text-sm">
              <Switch checked={field.in_standard_filter} onCheckedChange={(v) => onChange({ in_standard_filter: v })} /> In Filter
            </label>
            <label className="flex items-center gap-1.5 text-sm">
              <Switch checked={field.search_index} onCheckedChange={(v) => onChange({ search_index: v })} /> Search Index
            </label>
          </div>

          {/* Default & Description */}
          <div className="grid grid-cols-1 sm:grid-cols-2 gap-3 sm:gap-4 mt-4">
            <div>
              <Label>Default Value</Label>
              <Input
                value={field.default}
                onChange={(e) => onChange({ default: e.target.value })}
              />
            </div>
            <div>
              <Label>Description</Label>
              <Input
                value={field.description}
                onChange={(e) => onChange({ description: e.target.value })}
              />
            </div>
          </div>

          <div className="grid grid-cols-1 sm:grid-cols-2 gap-3 sm:gap-4 mt-4">
            <div>
              <Label>Show when</Label>
              <Input
                value={field.depends_on}
                onChange={(e) => onChange({ depends_on: e.target.value })}
                placeholder='status == "Open"'
              />
            </div>
            <div>
              <Label>Require when</Label>
              <Input
                value={field.mandatory_depends_on}
                onChange={(e) => onChange({ mandatory_depends_on: e.target.value })}
                placeholder='status == "Open"'
              />
            </div>
          </div>

          <div className="grid grid-cols-1 sm:grid-cols-2 gap-3 sm:gap-4 mt-4">
            <div>
              <Label>Dependency scope</Label>
              <Select value={field.dependency_scope || ''} onValueChange={(value) => onChange({ dependency_scope: value || '' })}>
                <SelectTrigger className="mt-1">
                  <SelectValue placeholder="Choose scope" />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="self">This record</SelectItem>
                  <SelectItem value="children">Child rows</SelectItem>
                  <SelectItem value="cross_doctype">Linked records</SelectItem>
                </SelectContent>
              </Select>
            </div>
            <div>
              <Label>Renamed from</Label>
              <Input
                value={field.renamed_from}
                onChange={(e) => onChange({ renamed_from: e.target.value })}
                placeholder="old_field_name"
              />
            </div>
          </div>

          {/* Linked Field */}
          {!isLayout && (
            <div className="mt-3">
              <Label>Linked Field (auto-populate from linked document)</Label>
              <Input
                className="mt-1"
                value={field.linked_field || ''}
                onChange={(e) => onChange({ linked_field: e.target.value })}
                placeholder="product.selling_price"
              />
            </div>
          )}

          {/* Computed */}
          {!isLayout && (
            <div className="mt-3">
              <Label>Computed Expression</Label>
              <LispAutocomplete
                className="mt-1 font-mono text-sm"
                value={field.computed || ''}
                onChange={(val) => onChange({ computed: val })}
                placeholder={field.computed?.startsWith('(') ? '(sum "items" "amount")' : 'quantity * unit_price'}
              />
            </div>
          )}

          {/* Constraints */}
          {!isLayout && (
            <div className="mt-3 border-t pt-3">
              <div className="flex items-center justify-between mb-2">
                <Label>Constraints</Label>
              </div>
              <ConstraintListEditor
                constraints={field.constraints || []}
                addLabel="Add constraint"
                emptyLabel="No field constraints yet."
                onChange={(next) => onChange({ constraints: next.length > 0 ? next : null })}
              />
            </div>
          )}
        </div>
      )}
    </div>
  )
}
