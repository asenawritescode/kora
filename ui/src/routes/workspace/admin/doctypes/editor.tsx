import { useNavigate, useRouterState } from '@tanstack/react-router'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useState, useCallback, useRef, useEffect } from 'react'
import { fetchDoctypes, createDoctype, updateDoctype } from '@/lib/api/system'
import type { DocType, Field } from '@/types/kora'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Switch } from '@/components/ui/switch'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Separator } from '@/components/ui/separator'
import { Skeleton } from '@/components/ui/skeleton'
import { ArrowLeft, Plus, ChevronDown, ChevronRight, GripVertical, Edit, Trash2, Save } from 'lucide-react'
import { YamlPanel } from '@/components/forms/YamlPanel'
import { LispAutocomplete } from '@/components/forms/LispAutocomplete'
import { Badge } from '@/components/ui/badge'

// --- Field type groups ---
const FIELD_TYPE_GROUPS: { label: string; types: string[] }[] = [
  { label: 'Text', types: ['Data', 'Text', 'Text Editor', 'Password', 'JSON'] },
  { label: 'Numbers', types: ['Int', 'Float', 'Currency', 'Percent'] },
  { label: 'Date & Time', types: ['Date', 'Time', 'Datetime'] },
  { label: 'Selection', types: ['Select', 'Check'] },
  { label: 'Relations', types: ['Link', 'Dynamic Link', 'Table'] },
  { label: 'Files', types: ['Attach', 'Attach Image'] },
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
}

const EMPTY_DOCTYPE: DocType = {
  name: '',
  module: '',
  is_submittable: false,
  is_child_table: false,
  is_single: false,
  track_changes: false,
  title_field: 'name',
  search_fields: 'name',
  sort_field: 'modified',
  sort_order: 'DESC',
  description: '',
  fields: [
    { ...EMPTY_FIELD, fieldname: 'name', fieldtype: 'Data', label: 'Name', reqd: true, in_list_view: true },
  ],
}

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

  // For edit mode, load from the doctypes list.
  const { data: doctypes } = useQuery({
    queryKey: ['admin', 'doctypes'],
    queryFn: fetchDoctypes,
    enabled: isEdit,
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
      if (isEdit) {
        await updateDoctype(doctypeName!, form, activate)
      } else {
        await createDoctype(form, activate)
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

  if (isEdit && loadingEdit) {
    return (
      <div className="p-8 space-y-4">
        <Skeleton className="h-8 w-64" />
        <Skeleton className="h-96 w-full" />
      </div>
    )
  }

  return (
    <div className="h-[calc(100vh-4rem)] flex flex-col overflow-hidden">
      {/* Header */}
      <div className="flex items-center justify-between px-3 sm:px-6 py-3 border-b shrink-0 gap-2">
        <div className="flex items-center gap-2 sm:gap-4 min-w-0">
          <Button variant="ghost" size="icon" className="h-8 w-8 shrink-0" onClick={() => navigate({ to: '/workspace/admin/doctypes' })}>
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
            Save Draft
          </Button>
          <Button size="sm" className="text-xs sm:text-sm h-8" onClick={() => handleSave(true)} disabled={saving}>
            <Save className="h-3.5 w-3.5 sm:mr-1" />
            <span className="hidden sm:inline">{currentStatus === 'Active' ? 'Save & Migrate' : 'Save & Activate'}</span>
          </Button>
        </div>
      </div>

      {error && (
        <div className="mx-4 mt-2 p-3 border border-destructive/50 bg-destructive/10 rounded-lg text-sm text-destructive">
          {error}
        </div>
      )}

      {/* Split pane: Form (left) + YAML (right) */}
      <div className="flex-1 flex overflow-hidden">
        {/* Form panel */}
        <div className="flex-1 overflow-y-auto p-4 sm:p-6 lg:p-8 space-y-6 sm:space-y-8">
        {/* Doctype Properties */}
        <section>
          <h2 className="text-lg font-semibold mb-4">Properties</h2>
          <div className="grid grid-cols-1 sm:grid-cols-2 gap-4">
            <div>
              <Label htmlFor="name">Name *</Label>
              <Input
                id="name"
                value={form.name}
                onChange={(e) => setForm({ ...form, name: e.target.value })}
                placeholder="Invoice"
                disabled={isEdit}
              />
            </div>
            <div>
              <Label htmlFor="module">Module *</Label>
              <Input
                id="module"
                value={form.module}
                onChange={(e) => setForm({ ...form, module: e.target.value })}
                placeholder="Billing"
              />
            </div>
            <div>
              <Label htmlFor="title_field">Title Field</Label>
              <Input
                id="title_field"
                value={form.title_field}
                onChange={(e) => setForm({ ...form, title_field: e.target.value })}
              />
            </div>
            <div>
              <Label htmlFor="search_fields">Search Fields</Label>
              <Input
                id="search_fields"
                value={form.search_fields}
                onChange={(e) => setForm({ ...form, search_fields: e.target.value })}
                placeholder="name, email"
              />
            </div>
            <div>
              <Label htmlFor="sort_field">Sort Field</Label>
              <Input
                id="sort_field"
                value={form.sort_field}
                onChange={(e) => setForm({ ...form, sort_field: e.target.value })}
              />
            </div>
            <div>
              <Label htmlFor="sort_order">Sort Order</Label>
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
              <span className="truncate">Submittable</span>
            </label>
            <label className="flex items-center gap-2 text-sm">
              <Switch checked={form.is_child_table} onCheckedChange={(v) => setForm({ ...form, is_child_table: v })} />
              <span className="truncate">Child Table</span>
            </label>
            <label className="flex items-center gap-2 text-sm">
              <Switch checked={form.is_single} onCheckedChange={(v) => setForm({ ...form, is_single: v })} />
              <span className="truncate">Single</span>
            </label>
            <label className="flex items-center gap-2 text-sm">
              <Switch checked={form.track_changes} onCheckedChange={(v) => setForm({ ...form, track_changes: v })} />
              <span className="truncate">Track Changes</span>
            </label>
          </div>
        </section>

        <Separator />

        {/* Fields */}
        <section>
          <div className="flex items-center justify-between mb-4">
            <h2 className="text-lg font-semibold">Fields</h2>
            <Button variant="outline" size="sm" onClick={addField}>
              <Plus className="h-4 w-4 mr-1" /> Add Field
            </Button>
          </div>

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
            <h2 className="text-lg font-semibold">Document Constraints</h2>
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
            <p className="text-sm text-muted-foreground italic">No document constraints defined.</p>
          )}
          {(form.doc_constraints || []).map((c, ci) => (
            <div key={ci} className="border rounded-lg p-3 mb-2">
              <div className="grid grid-cols-12 gap-2 items-start">
                <div className="col-span-3">
                  <Label className="text-xs">Type</Label>
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
                      <Label className="text-xs">Predicate (s-expression)</Label>
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
                      <Label className="text-xs">Condition (optional)</Label>
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
                      <Label className="text-xs">Value</Label>
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
                      <Label className="text-xs">Message</Label>
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
                  <Label className="text-xs">Message</Label>
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
          <Button variant="ghost" size="icon" className="h-6 w-6 hidden sm:inline-flex" onClick={() => onMoveUp()} disabled={!canMoveUp}>
            <ChevronRight className="h-3 w-3 rotate-[-90deg]" />
          </Button>
          <Button variant="ghost" size="icon" className="h-6 w-6 hidden sm:inline-flex" onClick={() => onMoveDown()} disabled={!canMoveDown}>
            <ChevronRight className="h-3 w-3 rotate-90" />
          </Button>
          <Button variant="ghost" size="icon" className="h-6 w-6" onClick={onToggle}>
            <Edit className="h-3 w-3" />
          </Button>
          <Button variant="ghost" size="icon" className="h-6 w-6" onClick={onRemove}>
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
                placeholder="Order Item"
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
                <Button
                  variant="ghost" size="sm"
                  onClick={() => {
                    const updated = [...(field.constraints || []), { type: 'max', value: undefined as any, message: '' }]
                    onChange({ constraints: updated })
                  }}
                >+ Add</Button>
              </div>
              {(field.constraints || []).map((c, ci) => (
                <div key={ci} className="grid grid-cols-12 gap-2 mb-2 items-start">
                  <select
                    className="col-span-3 h-9 rounded-md border bg-background px-2 text-sm"
                    value={c.type}
                    onChange={(e) => {
                      const updated = [...(field.constraints || [])]
                      updated[ci] = { ...updated[ci], type: e.target.value }
                      onChange({ constraints: updated })
                    }}
                  >
                    <option value="max">max</option>
                    <option value="min">min</option>
                    <option value="max_length">max_length</option>
                    <option value="min_length">min_length</option>
                    <option value="max_rows">max_rows</option>
                    <option value="min_rows">min_rows</option>
                    <option value="regex">regex</option>
                    <option value="one_of">one_of</option>
                    <option value="not_one_of">not_one_of</option>
                    <option value="min_date">min_date</option>
                    <option value="max_date">max_date</option>
                  </select>
                  <Input
                    className="col-span-3 h-9 text-sm"
                    value={c.value != null ? String(c.value) : ''}
                    onChange={(e) => {
                      const v = e.target.value
                      const num = Number(v)
                      const updated = [...(field.constraints || [])]
                      updated[ci] = { ...updated[ci], value: isNaN(num) ? v : num }
                      onChange({ constraints: updated })
                    }}
                    placeholder="value"
                  />
                  <Input
                    className="col-span-5 h-9 text-sm"
                    value={c.message || ''}
                    onChange={(e) => {
                      const updated = [...(field.constraints || [])]
                      updated[ci] = { ...updated[ci], message: e.target.value }
                      onChange({ constraints: updated })
                    }}
                    placeholder="Error message"
                  />
                  <Button
                    variant="ghost" size="sm"
                    className="col-span-1 h-9 text-destructive"
                    onClick={() => {
                      const updated = (field.constraints || []).filter((_, i) => i !== ci)
                      onChange({ constraints: updated.length > 0 ? updated : null })
                    }}
                  >✕</Button>
                </div>
              ))}
            </div>
          )}
        </div>
      )}
    </div>
  )
}
