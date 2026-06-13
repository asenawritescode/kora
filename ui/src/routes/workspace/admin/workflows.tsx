import { useQuery } from '@tanstack/react-query'
import { useState } from 'react'
import { fetchWorkflows, fetchWorkflow, saveWorkflow, deleteWorkflow } from '@/lib/api/system'
import { fetchDoctypes } from '@/lib/api/system'
import type { WorkflowDef, WorkflowState, WorkflowTransition, WorkflowNotification } from '@/lib/api/system'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Switch } from '@/components/ui/switch'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Skeleton } from '@/components/ui/skeleton'
import { Separator } from '@/components/ui/separator'
import { Link } from '@tanstack/react-router'
import { Workflow, Plus, Edit, Trash2, Save, ArrowLeft, ChevronDown, ChevronRight } from 'lucide-react'

const EMPTY_STATE: WorkflowState = { state: '', doc_status: 0, allow_edit: '', style: 'default' }
const EMPTY_TRANSITION: WorkflowTransition = { action: '', from: '', to: '', allowed: '', condition: '', require_fields: [] }
const EMPTY_NOTIFICATION: WorkflowNotification = { event: 'state_change', to_state: '', recipients: [{ field: '' }], subject: '', message: '' }
const STYLES = ['default', 'warning', 'success', 'danger', 'info']

export default function AdminWorkflowsPage() {
  const [editingDocType, setEditingDocType] = useState<string | null>(null)
  const [form, setForm] = useState<WorkflowDef | null>(null)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [expanded, setExpanded] = useState<Record<string, boolean>>({ states: true, transitions: true, notifications: true })

  const toggle = (key: string) => setExpanded((p) => ({ ...p, [key]: !p[key] }))

  const { data: workflows, isLoading, refetch } = useQuery({ queryKey: ['admin', 'workflows'], queryFn: fetchWorkflows })
  const { data: doctypes } = useQuery({ queryKey: ['admin', 'doctypes'], queryFn: fetchDoctypes })
  const allDocs = doctypes?.filter((d: any) => !d.is_child_table) || []

  const startNew = (doctype: string) => {
    setForm({
      name: `${doctype} Workflow`, document_type: doctype, is_active: true, workflow_state_field: 'status',
      states: [{ state: 'Draft', doc_status: 0, allow_edit: '', style: 'default' }],
      transitions: [], notifications: [],
    })
    setEditingDocType(doctype); setError(null)
  }

  const startEdit = async (doctype: string) => {
    try {
      const wf = await fetchWorkflow(doctype)
      setForm({ ...wf, notifications: wf.notifications || [] })
      setEditingDocType(doctype); setError(null)
    } catch (e) { alert((e as Error).message) }
  }

  const handleSave = async () => {
    if (!form) return; setSaving(true); setError(null)
    try { await saveWorkflow(form); refetch(); setEditingDocType(null); setForm(null) }
    catch (e) { setError((e as Error).message) }
    finally { setSaving(false) }
  }

  const handleDelete = async (doctype: string) => {
    if (!confirm(`Delete workflow for ${doctype}?`)) return
    try { await deleteWorkflow(doctype); refetch() } catch (e) { alert((e as Error).message) }
  }

  // --- Editor view ---
  if (editingDocType && form) {
    return (
      <div className="flex flex-col h-[calc(100vh-4rem)]">
        {/* Header */}
        <div className="flex items-center justify-between px-3 sm:px-6 py-3 border-b shrink-0 gap-2">
          <div className="flex items-center gap-2 sm:gap-3 min-w-0">
            <Button variant="ghost" size="icon" className="h-8 w-8 shrink-0" onClick={() => { setEditingDocType(null); setForm(null) }}>
              <ArrowLeft className="h-4 w-4" />
            </Button>
            <div className="min-w-0">
              <h1 className="text-base sm:text-xl font-bold truncate">{form.name}</h1>
              <p className="text-xs text-muted-foreground hidden sm:block">{form.document_type}</p>
            </div>
          </div>
          <Button size="sm" className="h-8 shrink-0" onClick={handleSave} disabled={saving}>
            <Save className="h-3.5 w-3.5 sm:mr-1" /><span className="hidden sm:inline">{saving ? 'Saving...' : 'Save'}</span>
          </Button>
        </div>

        {error && <div className="mx-3 mt-2 p-3 border border-destructive/50 bg-destructive/10 rounded-lg text-sm text-destructive">{error}</div>}

        <div className="flex-1 overflow-y-auto p-4 sm:p-6 space-y-6">
          {/* Properties */}
          <div className="grid grid-cols-1 sm:grid-cols-2 gap-3">
            <div><Label>Name</Label><Input className="h-8 mt-1" value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} /></div>
            <div><Label>DocType</Label><Input className="h-8 mt-1" value={form.document_type} disabled /></div>
            <div><Label>State Field</Label><Input className="h-8 mt-1" value={form.workflow_state_field} onChange={(e) => setForm({ ...form, workflow_state_field: e.target.value })} /></div>
            <label className="flex items-center gap-2 pt-5"><Switch checked={form.is_active} onCheckedChange={(v) => setForm({ ...form, is_active: v })} /> Active</label>
          </div>

          <Separator />

          {/* STATES */}
          <Section title="States" count={form.states.length} onAdd={() => setForm({ ...form, states: [...form.states, { ...EMPTY_STATE }] })} expanded={expanded['states']} onToggle={() => toggle('states')} defaultExpanded>
            {form.states.map((s, i) => (
              <StateCard key={i} state={s} index={i} onChange={(upd) => { const st = [...form.states]; st[i] = { ...st[i], ...upd }; setForm({ ...form, states: st }) }} onRemove={() => setForm({ ...form, states: form.states.filter((_, j) => j !== i) })} />
            ))}
          </Section>

          {/* TRANSITIONS */}
          <Section title="Transitions" count={form.transitions.length} onAdd={() => setForm({ ...form, transitions: [...form.transitions, { ...EMPTY_TRANSITION }] })} expanded={expanded['transitions']} onToggle={() => toggle('transitions')}>
            {form.transitions.map((t, i) => (
              <TransitionCard key={i} t={t} states={form.states} index={i} onChange={(upd) => { const tr = [...form.transitions]; tr[i] = { ...tr[i], ...upd }; setForm({ ...form, transitions: tr }) }} onRemove={() => setForm({ ...form, transitions: form.transitions.filter((_, j) => j !== i) })} />
            ))}
          </Section>

          {/* NOTIFICATIONS */}
          <Section title="Notifications" count={form.notifications.length} onAdd={() => setForm({ ...form, notifications: [...form.notifications, { ...EMPTY_NOTIFICATION, recipients: [{ field: '' }] }] })} expanded={expanded['notifications']} onToggle={() => toggle('notifications')}>
            {form.notifications.map((n, i) => (
              <NotificationCard key={i} n={n} index={i} onChange={(upd) => { const nf = [...form.notifications]; nf[i] = { ...nf[i], ...upd }; setForm({ ...form, notifications: nf }) }} onRemove={() => setForm({ ...form, notifications: form.notifications.filter((_, j) => j !== i) })} />
            ))}
          </Section>
        </div>
      </div>
    )
  }

  // --- List view ---
  return (
    <div className="p-4 sm:p-8 max-w-5xl">
      <div className="flex items-center gap-3 mb-6">
        <Workflow className="h-6 w-6" />
        <div>
          <h1 className="text-2xl sm:text-3xl font-bold tracking-tight">Workflows</h1>
          <p className="text-muted-foreground mt-1 text-sm">Define document lifecycles</p>
        </div>
      </div>

      {isLoading && <div className="space-y-2">{[1, 2, 3].map((i) => <Skeleton key={i} className="h-16 w-full" />)}</div>}

      {!isLoading && allDocs.length === 0 && (
        <div className="border-2 border-dashed rounded-lg p-8 sm:p-12 text-center">
          <Workflow className="h-12 w-12 mx-auto text-muted-foreground/40" />
          <h3 className="text-lg font-semibold mt-4">No doctypes yet</h3>
          <p className="text-muted-foreground mt-1 text-sm max-w-md mx-auto">
            Create a doctype first, then return here to define its workflow.
          </p>
          <Link to="/workspace/admin/doctypes/new">
            <Button variant="outline" className="mt-4">Create a DocType</Button>
          </Link>
        </div>
      )}

      {!isLoading && allDocs.length > 0 && (
        <div className="space-y-3">
          {allDocs.map((dt: any) => {
            const existingWf = workflows?.find((w: WorkflowDef) => w.document_type === dt.name)
            return (
              <div key={dt.name} className="border rounded-lg p-4 space-y-3">
                <div>
                  <div className="font-medium flex items-center gap-2">
                    {dt.name}
                    {!dt.is_submittable && <span className="text-[10px] bg-amber-100 text-amber-800 px-1.5 py-0.5 rounded-full font-medium">Not submittable</span>}
                  </div>
                  <div className="text-xs text-muted-foreground mt-0.5">
                    {existingWf
                      ? `${existingWf.states?.length || 0} states, ${existingWf.transitions?.length || 0} transitions`
                      : !dt.is_submittable
                        ? 'Enable "Is Submittable" in DocType editor first'
                        : 'No workflow'}
                  </div>
                </div>
                <div className="flex gap-2">
                  {existingWf ? (
                    <>
                      <Button variant="outline" size="sm" onClick={() => startEdit(dt.name)}><Edit className="h-4 w-4 sm:mr-1" /><span className="hidden sm:inline">Edit</span></Button>
                      <Button variant="ghost" size="sm" onClick={() => handleDelete(dt.name)}><Trash2 className="h-4 w-4 text-destructive" /></Button>
                    </>
                  ) : (
                    <Button variant="outline" size="sm" onClick={() => startNew(dt.name)}><Plus className="h-4 w-4 sm:mr-1" /><span className="hidden sm:inline">Create</span></Button>
                  )}
                </div>
              </div>
            )
          })}
        </div>
      )}
    </div>
  )
}

// --- Reusable collapsible section ---
function Section({ title, count, onAdd, expanded, onToggle, defaultExpanded, children }: {
  title: string; count: number; onAdd: () => void; expanded?: boolean; onToggle?: () => void; defaultExpanded?: boolean; children: React.ReactNode
}) {
  const isExpanded = expanded ?? true
  return (
    <section>
      <div className="flex items-center justify-between mb-3">
        <button className="flex items-center gap-2 text-left" onClick={onToggle}>
          {onToggle && (isExpanded ? <ChevronDown className="h-4 w-4" /> : <ChevronRight className="h-4 w-4" />)}
          <h2 className="text-lg font-semibold">{title}</h2>
          <span className="text-xs text-muted-foreground">({count})</span>
        </button>
        <Button variant="outline" size="sm" onClick={onAdd}><Plus className="h-4 w-4 sm:mr-1" /><span className="hidden sm:inline">Add</span></Button>
      </div>
      {isExpanded && <div className="space-y-3">{children}</div>}
    </section>
  )
}

// --- State card ---
function StateCard({ state: s, index, onChange, onRemove }: {
  state: WorkflowState; index: number; onChange: (u: Partial<WorkflowState>) => void; onRemove: () => void
}) {
  return (
    <div className="border rounded-lg p-4 space-y-3">
      <div className="grid grid-cols-1 sm:grid-cols-4 gap-3">
        <div><Label className="sm:hidden text-xs text-muted-foreground">State</Label><Input className="h-8" value={s.state} onChange={(e) => onChange({ state: e.target.value })} placeholder="State name" /></div>
        <div><Label className="sm:hidden text-xs text-muted-foreground">Doc Status</Label><Input className="h-8" type="number" value={s.doc_status} onChange={(e) => onChange({ doc_status: parseInt(e.target.value) || 0 })} /></div>
        <div><Label className="sm:hidden text-xs text-muted-foreground">Allow Edit</Label><Input className="h-8" value={s.allow_edit} onChange={(e) => onChange({ allow_edit: e.target.value })} placeholder="Role" /></div>
        <div className="flex gap-2 items-end">
          <div className="flex-1">
            <Label className="sm:hidden text-xs text-muted-foreground">Style</Label>
            <Select value={s.style} onValueChange={(v) => onChange({ style: v || 'default' })}>
              <SelectTrigger className="h-8"><SelectValue /></SelectTrigger>
              <SelectContent>{STYLES.map((st) => <SelectItem key={st} value={st}>{st}</SelectItem>)}</SelectContent>
            </Select>
          </div>
          <Button variant="ghost" size="icon" className="h-8 w-8 shrink-0" onClick={onRemove}><Trash2 className="h-4 w-4 text-destructive" /></Button>
        </div>
      </div>
    </div>
  )
}

// --- Transition card ---
function TransitionCard({ t, states, onChange, onRemove }: {
  t: WorkflowTransition; states: WorkflowState[]; index: number; onChange: (u: Partial<WorkflowTransition>) => void; onRemove: () => void
}) {
  return (
    <div className="border rounded-lg p-4 space-y-3">
      <div className="flex items-center gap-2">
        <Select value={t.from} onValueChange={(v) => onChange({ from: v || '' })}>
          <SelectTrigger className="h-8 flex-1"><SelectValue placeholder="From" /></SelectTrigger>
          <SelectContent>
            <SelectItem value="__any__">Any State</SelectItem>
            {states.map((s) => <SelectItem key={s.state} value={s.state}>{s.state}</SelectItem>)}
          </SelectContent>
        </Select>
        <span className="text-muted-foreground text-sm shrink-0">→</span>
        <Select value={t.to} onValueChange={(v) => onChange({ to: v || '' })}>
          <SelectTrigger className="h-8 flex-1"><SelectValue placeholder="To" /></SelectTrigger>
          <SelectContent>{states.map((s) => <SelectItem key={s.state} value={s.state}>{s.state}</SelectItem>)}</SelectContent>
        </Select>
        <Button variant="ghost" size="icon" className="h-8 w-8 shrink-0" onClick={onRemove}><Trash2 className="h-4 w-4 text-destructive" /></Button>
      </div>
      <div className="grid grid-cols-1 sm:grid-cols-3 gap-3">
        <div><Label className="sm:hidden text-xs text-muted-foreground">Action</Label><Input className="h-8" value={t.action} onChange={(e) => onChange({ action: e.target.value })} placeholder="Action label" /></div>
        <div><Label className="sm:hidden text-xs text-muted-foreground">Allowed Roles</Label><Input className="h-8" value={t.allowed} onChange={(e) => onChange({ allowed: e.target.value })} placeholder="Admin, Sales" /></div>
        <div><Label className="sm:hidden text-xs text-muted-foreground">Condition</Label><Input className="h-8" value={t.condition || ''} onChange={(e) => onChange({ condition: e.target.value })} placeholder="Optional" /></div>
      </div>
    </div>
  )
}

// --- Notification card ---
function NotificationCard({ n, onChange, onRemove }: {
  n: WorkflowNotification; index: number; onChange: (u: Partial<WorkflowNotification>) => void; onRemove: () => void
}) {
  return (
    <div className="border rounded-lg p-4 space-y-3">
      <div className="grid grid-cols-1 sm:grid-cols-3 gap-3">
        <div><Label className="sm:hidden text-xs text-muted-foreground">Event</Label><Input className="h-8" value={n.event} onChange={(e) => onChange({ event: e.target.value })} /></div>
        <div><Label className="sm:hidden text-xs text-muted-foreground">To State</Label><Input className="h-8" value={n.to_state || ''} onChange={(e) => onChange({ to_state: e.target.value })} placeholder="Any" /></div>
        <div><Label className="sm:hidden text-xs text-muted-foreground">Recipient Field</Label><Input className="h-8" value={n.recipients[0]?.field || ''} onChange={(e) => onChange({ recipients: [{ field: e.target.value }] })} placeholder="customer.email" /></div>
        <div className="sm:col-span-2"><Label className="sm:hidden text-xs text-muted-foreground">Subject</Label><Input className="h-8" value={n.subject} onChange={(e) => onChange({ subject: e.target.value })} placeholder="Your order {name} has shipped" /></div>
        <div className="flex items-end"><Button variant="ghost" size="sm" onClick={onRemove}><Trash2 className="h-4 w-4 mr-1 text-destructive" /> Remove</Button></div>
      </div>
    </div>
  )
}
