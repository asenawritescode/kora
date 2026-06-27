import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useState, useEffect, useMemo } from 'react'
import {
  fetchScripts, fetchScript, fetchScriptExecutions,
  createScript, updateScript, deleteScript,
} from '@/lib/api/scripts'
import { fetchDoctypes, fetchUsers } from '@/lib/api/system'
import type { ScriptRecord, ScriptExecution } from '@/lib/api/scripts'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Skeleton } from '@/components/ui/skeleton'
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from '@/components/ui/sheet'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Badge } from '@/components/ui/badge'
import { Switch } from '@/components/ui/switch'
import {
  Code2, Plus, Pencil, Trash2, FileCheck, History, AlertCircle,
  Loader2, CheckCircle, XCircle, Clock,
} from 'lucide-react'
import { ConfirmDialog } from '@/components/ui/confirm-dialog'
import { CodeEditor } from '@/components/forms/CodeEditor'

function useIsMobile() {
  const [isMobile, setIsMobile] = useState(false)
  useEffect(() => {
    const check = () => setIsMobile(window.innerWidth < 768)
    check()
    window.addEventListener('resize', check)
    return () => window.removeEventListener('resize', check)
  }, [])
  return isMobile
}

const SCRIPT_TYPES = [
  { value: 'doc_event', label: 'Doc Event' },
  { value: 'api_method', label: 'API Method' },
  { value: 'workflow_action', label: 'Workflow Action' },
  { value: 'scheduled', label: 'Scheduled' },
]

const DOC_EVENTS = [
  { value: 'before_insert', label: 'Before Insert' },
  { value: 'after_insert', label: 'After Insert' },
  { value: 'before_save', label: 'Before Save' },
  { value: 'after_save', label: 'After Save' },
  { value: 'before_delete', label: 'Before Delete' },
  { value: 'after_delete', label: 'After Delete' },
  { value: 'before_submit', label: 'Before Submit' },
  { value: 'after_submit', label: 'After Submit' },
  { value: 'before_cancel', label: 'Before Cancel' },
  { value: 'after_cancel', label: 'After Cancel' },
  { value: 'validate', label: 'Validate' },
]

export default function AdminScriptsPage() {
  const isMobile = useIsMobile()
  const queryClient = useQueryClient()
  const { data: scripts, isLoading } = useQuery({
    queryKey: ['admin', 'scripts'],
    queryFn: fetchScripts,
  })

  const { data: doctypes } = useQuery({
    queryKey: ['admin', 'doctypes'],
    queryFn: fetchDoctypes,
  })
  const doctypeNames = (doctypes as any[])?.map((d: any) => d.name) || []

  const { data: users } = useQuery({
    queryKey: ['admin', 'users'],
    queryFn: fetchUsers as any,
  })
  const userEmails = (users as any[])?.map((u: any) => u.email).filter(Boolean) || []

  const [dialogOpen, setDialogOpen] = useState(false)
  const [editName, setEditName] = useState('')
  const [formData, setFormData] = useState<{
    name: string; script_type: string; doctype: string; event: string;
    method_path: string; workflow_action: string; schedule: string;
    priority: number; timeout_ms: number; run_as: string; script: string;
  }>({
    name: '', script_type: 'doc_event', doctype: '', event: '',
    method_path: '', workflow_action: '', schedule: '',
    priority: 10, timeout_ms: 5000, run_as: '', script: '// Write your JavaScript here\n',
  })
  const [saving, setSaving] = useState(false)
  const [formError, setFormError] = useState('')

  const [executionsFor, setExecutionsFor] = useState<string | null>(null)
  const { data: executions } = useQuery({
    queryKey: ['admin', 'scripts', executionsFor, 'executions'],
    queryFn: () => fetchScriptExecutions(executionsFor!),
    enabled: !!executionsFor,
  })

  const [confirmDelete, setConfirmDelete] = useState<string | null>(null)
  const [validationResult, setValidationResult] = useState<{ status: string; output?: string; error?: string; duration?: number } | null>(null)
  const [validating, setValidating] = useState(false)
  const [logFilter, setLogFilter] = useState<'all' | 'success' | 'error'>('all')
  const [expandedErrorId, setExpandedErrorId] = useState<string | null>(null)

  const openCreate = () => {
    setEditName('')
    setFormData({ name: '', script_type: 'doc_event', doctype: '', event: '',
      method_path: '', workflow_action: '', schedule: '', priority: 10, timeout_ms: 5000, run_as: '', script: '// Write your JavaScript here\n' })
    setFormError('')
    setDialogOpen(true)
  }

  const openEdit = (s: ScriptRecord) => {
    setEditName(s.name)
    setFormData({
      name: s.name, script_type: s.script_type, doctype: s.doctype, event: s.event,
      method_path: s.method_path, workflow_action: s.workflow_action, schedule: s.schedule,
      priority: s.priority, timeout_ms: s.timeout_ms, run_as: s.run_as, script: s.script,
    })
    setFormError('')
    setDialogOpen(true)
  }

  const handleSave = async (e: React.FormEvent) => {
    e.preventDefault()
    setFormError('')
    setSaving(true)
    try {
      if (editName) {
        await updateScript(editName, {
          script_type: formData.script_type, doctype: formData.doctype, event: formData.event,
          method_path: formData.method_path, workflow_action: formData.workflow_action,
          schedule: formData.schedule, priority: formData.priority, timeout_ms: formData.timeout_ms,
          run_as: formData.run_as, script: formData.script,
        })
      } else {
        await createScript({
          name: formData.name, script_type: formData.script_type, doctype: formData.doctype,
          event: formData.event, method_path: formData.method_path,
          workflow_action: formData.workflow_action, schedule: formData.schedule,
          priority: formData.priority, timeout_ms: formData.timeout_ms,
          run_as: formData.run_as, script: formData.script,
        })
      }
      setDialogOpen(false)
      queryClient.invalidateQueries({ queryKey: ['admin', 'scripts'] })
    } catch (err: any) {
      setFormError(err?.message || 'Failed to save script')
    } finally {
      setSaving(false)
    }
  }

  const handleDelete = async (name: string) => {
    setConfirmDelete(name)
  }

  const handleToggleActive = async (s: ScriptRecord) => {
    await updateScript(s.name, { is_active: !s.is_active })
    queryClient.invalidateQueries({ queryKey: ['admin', 'scripts'] })
  }

  const handleValidate = async () => {
    setValidating(true)
    setValidationResult(null)
    try {
      const res = await fetch('/api/system/script/_validate', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ script: formData.script }),
      })
      const data = await res.json()
      if (data.data?.valid) {
        setValidationResult({ status: 'success', output: 'Syntax is valid.', duration: 0 })
      } else {
        setValidationResult({ status: 'error', error: data.data?.error || 'Unknown error' })
      }
    } catch (err: any) {
      setValidationResult({ status: 'error', error: err?.message || 'Validation failed' })
    } finally {
      setValidating(false)
    }
  }

  const scriptType = formData.script_type
  const typeLabel = SCRIPT_TYPES.find(t => t.value === scriptType)?.label || scriptType

  if (isLoading) return <div className="p-6 space-y-4"><Skeleton className="h-8 w-64" /><Skeleton className="h-64 w-full" /></div>

  return (
    <div className="p-6 space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-bold tracking-tight flex items-center gap-2">
            <Code2 className="h-6 w-6" /> Scripts
          </h1>
          <p className="text-muted-foreground mt-1">JavaScript hooks, custom API methods, and workflow actions.</p>
        </div>
        <Button onClick={openCreate}><Plus className="h-4 w-4 mr-2" /> New Script</Button>
      </div>

      {/* Script list — desktop table */}
      <Card>
        <CardContent className="p-0 hidden md:block">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead>Type</TableHead>
                <TableHead>Doctype / Path</TableHead>
                <TableHead>Event / Method</TableHead>
                <TableHead>Status</TableHead>
                <TableHead className="w-[120px]">Actions</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {(!scripts || scripts.length === 0) && (
                <TableRow><TableCell colSpan={6} className="text-center text-muted-foreground py-8">
                  No scripts yet. Create one to get started.
                </TableCell></TableRow>
              )}
              {scripts?.map((s) => (
                <TableRow key={s.name}>
                  <TableCell className="font-medium">{s.name}</TableCell>
                  <TableCell><Badge variant="outline">{s.script_type}</Badge></TableCell>
                  <TableCell className="text-sm text-muted-foreground">
                    {s.doctype || s.method_path || s.workflow_action || s.schedule || '—'}
                  </TableCell>
                  <TableCell className="text-sm text-muted-foreground">{s.event || '—'}</TableCell>
                  <TableCell>
                    <div className="flex items-center gap-2">
                      <Switch checked={s.is_active} onCheckedChange={() => handleToggleActive(s)} />
                      <span className="text-xs text-muted-foreground">{s.is_active ? 'Active' : 'Inactive'}</span>
                    </div>
                  </TableCell>
                  <TableCell>
                    <div className="flex gap-1">
                      <Button variant="ghost" size="icon" title="Edit" onClick={() => openEdit(s)}><Pencil className="h-4 w-4" /></Button>
                      <Button variant="ghost" size="icon" title="Executions" onClick={() => setExecutionsFor(executionsFor === s.name ? null : s.name)}>
                        <History className="h-4 w-4" />
                      </Button>
                      <Button variant="ghost" size="icon" title="Delete" onClick={() => handleDelete(s.name)}><Trash2 className="h-4 w-4 text-red-500" /></Button>
                    </div>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </CardContent>

        {/* Script list — mobile stacked cards */}
        <CardContent className="md:hidden p-0">
        <div className="divide-y">
          {(!scripts || scripts.length === 0) && (
            <div className="text-center text-muted-foreground py-8 text-sm">No scripts yet. Create one to get started.</div>
          )}
          {scripts?.map((s) => (
            <div key={s.name} className="p-4 space-y-2">
              <div className="flex items-center justify-between">
                <div className="flex items-center gap-2">
                  <span className="font-medium text-sm">{s.name}</span>
                  <Badge variant="outline" className="text-xs">{s.script_type}</Badge>
                </div>
                <Switch checked={s.is_active} onCheckedChange={() => handleToggleActive(s)} />
              </div>
              <div className="flex flex-wrap gap-2 text-xs text-muted-foreground">
                {s.doctype && <span className="bg-muted px-1.5 py-0.5 rounded">Doctype: {s.doctype}</span>}
                {s.event && <span className="bg-muted px-1.5 py-0.5 rounded">Event: {s.event}</span>}
                {s.method_path && <span className="bg-muted px-1.5 py-0.5 rounded">Path: {s.method_path}</span>}
                {s.workflow_action && <span className="bg-muted px-1.5 py-0.5 rounded">Action: {s.workflow_action}</span>}
                {s.schedule && <span className="bg-muted px-1.5 py-0.5 rounded">Cron: {s.schedule}</span>}
              </div>
              <div className="flex gap-1 justify-end">
                <Button variant="ghost" size="sm" onClick={() => openEdit(s)}><Pencil className="h-3.5 w-3.5 mr-1" />Edit</Button>
                <Button variant="ghost" size="sm" onClick={() => setExecutionsFor(executionsFor === s.name ? null : s.name)}><History className="h-3.5 w-3.5 mr-1" />Log</Button>
                <Button variant="ghost" size="sm" onClick={() => handleDelete(s.name)}><Trash2 className="h-3.5 w-3.5 mr-1 text-red-500" />Delete</Button>
              </div>
            </div>
          ))}
        </div>
        </CardContent>
      </Card>

      {/* Execution log */}
      {executionsFor && (() => {
        const filteredExecutions = executions?.filter(e => {
          if (logFilter === 'all') return true
          return e.status === logFilter
        }) || []
        return (
        <Card>
          <CardHeader>
            <CardTitle className="text-lg flex items-center gap-2">
              <History className="h-5 w-5" /> Executions: {executionsFor}
            </CardTitle>
            <CardDescription>Recent script execution log.</CardDescription>
          </CardHeader>
          <CardContent>
            <div className="flex items-center gap-2 mb-3">
              <Button variant={logFilter === 'all' ? 'default' : 'outline'} size="sm" onClick={() => setLogFilter('all')}>All</Button>
              <Button variant={logFilter === 'success' ? 'default' : 'outline'} size="sm" onClick={() => setLogFilter('success')}>Success</Button>
              <Button variant={logFilter === 'error' ? 'default' : 'outline'} size="sm" onClick={() => setLogFilter('error')}>Errors</Button>
              <span className="text-xs text-muted-foreground ml-2">
                Showing {filteredExecutions.length} of {executions?.length || 0} executions
              </span>
            </div>
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Time</TableHead>
                  <TableHead>Doctype</TableHead>
                  <TableHead>Document</TableHead>
                  <TableHead>Event</TableHead>
                  <TableHead>Duration</TableHead>
                  <TableHead>Status</TableHead>
                  <TableHead>Error</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {(!executions || executions.length === 0) && (
                  <TableRow><TableCell colSpan={7} className="text-center text-muted-foreground py-4">No executions yet.</TableCell></TableRow>
                )}
                {filteredExecutions.length === 0 && (executions?.length || 0) > 0 && (
                  <TableRow><TableCell colSpan={7} className="text-center text-muted-foreground py-4">No executions match the filter.</TableCell></TableRow>
                )}
                {filteredExecutions.map((e) => (
                  <TableRow key={e.id} className={e.status === 'error' ? 'cursor-pointer hover:bg-red-50/50' : ''} onClick={() => e.status === 'error' && setExpandedErrorId(expandedErrorId === e.id ? null : e.id)}>
                    <TableCell className="text-xs text-muted-foreground">{new Date(e.logged_at).toLocaleString()}</TableCell>
                    <TableCell className="text-xs">{e.doctype || '—'}</TableCell>
                    <TableCell className="text-xs font-mono">{e.docname || '—'}</TableCell>
                    <TableCell className="text-xs">{e.event || '—'}</TableCell>
                    <TableCell className="text-xs">{e.duration_ms}ms</TableCell>
                    <TableCell>
                      {e.status === 'success' ? <CheckCircle className="h-4 w-4 text-green-500" /> :
                       e.status === 'error' ? <XCircle className="h-4 w-4 text-red-500" /> :
                       <Clock className="h-4 w-4 text-yellow-500" />}
                    </TableCell>
                    <TableCell className={`text-xs text-red-500 max-w-[200px] ${expandedErrorId === e.id ? 'whitespace-pre-wrap' : 'truncate'}`}>{e.error_message || ''}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </CardContent>
        </Card>
        )
      })()}

      {/* Create/Edit Sheet */}
      <Sheet open={dialogOpen} onOpenChange={setDialogOpen}>
        <SheetContent side={isMobile ? 'bottom' : 'right'} className={isMobile ? 'w-full max-h-[85vh] rounded-t-xl flex flex-col' : 'w-full sm:max-w-xl flex flex-col'}>
          <SheetHeader className="border-b pb-4">
            <SheetTitle className="text-lg">{editName ? `Edit: ${editName}` : 'New Script'}</SheetTitle>
            <SheetDescription>Scripts run inside the Kora engine with access to <code>kora.*</code> APIs.</SheetDescription>
          </SheetHeader>
          <form onSubmit={handleSave} className="flex-1 flex flex-col min-h-0">
            <div className="flex-1 overflow-y-auto p-4 space-y-5">
            {/* Name */}
            {!editName && (
              <div className="space-y-1.5">
                <Label htmlFor="name" className="text-sm font-medium">Name <span className="text-red-500">*</span></Label>
                <Input id="name" value={formData.name} onChange={e => setFormData({...formData, name: e.target.value})}
                  placeholder="e.g., credit_check" required />
              </div>
            )}

            {/* Type */}
            <div className="space-y-1.5">
              <Label className="text-sm font-medium">Script Type</Label>
              <Select value={formData.script_type} onValueChange={(v) => { const val = v || 'doc_event'; setFormData({...formData, script_type: val}); }}>
                <SelectTrigger><SelectValue /></SelectTrigger>
                <SelectContent>{SCRIPT_TYPES.map(t => <SelectItem key={t.value} value={t.value}>{t.label}</SelectItem>)}</SelectContent>
              </Select>
            </div>

            {/* Type-specific fields */}
            {scriptType === 'doc_event' && (
              <div className="grid grid-cols-2 gap-4">
                <div className="grid gap-2">
                  <Label>DocType</Label>
                  <Select value={formData.doctype} onValueChange={(v) => setFormData({...formData, doctype: v || ''})}>
                    <SelectTrigger><SelectValue placeholder="Select doctype..." /></SelectTrigger>
                    <SelectContent>
                      {doctypeNames.length === 0 && <SelectItem value="_loading" disabled>Loading...</SelectItem>}
                      {doctypeNames.map((name: string) => <SelectItem key={name} value={name}>{name}</SelectItem>)}
                    </SelectContent>
                  </Select>
                </div>
                <div className="grid gap-2">
                  <Label>Event</Label>
                  <Select value={formData.event} onValueChange={(v) => { setFormData({...formData, event: v || ''}); }}>
                    <SelectTrigger><SelectValue placeholder="Select event..." /></SelectTrigger>
                    <SelectContent>{DOC_EVENTS.map(ev => <SelectItem key={ev.value} value={ev.value}>{ev.label}</SelectItem>)}</SelectContent>
                  </Select>
                </div>
              </div>
            )}
            {scriptType === 'api_method' && (
              <div className="grid gap-2">
                <Label>Method Path</Label>
                <Input value={formData.method_path} onChange={e => setFormData({...formData, method_path: e.target.value})}
                  placeholder="e.g., mpesa.callback" />
                <p className="text-xs text-muted-foreground">Accessible at POST /api/method/{'{name}'}</p>
              </div>
            )}
            {scriptType === 'workflow_action' && (
              <div className="grid gap-2">
                <Label>Action Name</Label>
                <Input value={formData.workflow_action} onChange={e => setFormData({...formData, workflow_action: e.target.value})}
                  placeholder="e.g., create_service_report" />
              </div>
            )}
            {scriptType === 'scheduled' && (
              <div className="grid grid-cols-2 gap-4">
                <div className="grid gap-2">
                  <Label>Schedule (Cron)</Label>
                  <Input value={formData.schedule} onChange={e => setFormData({...formData, schedule: e.target.value})}
                    placeholder="0 7 * * *" />
                </div>
              </div>
            )}

            {/* Options */}
            <div className="grid grid-cols-3 gap-4">
              <div className="grid gap-2">
                <Label>Priority</Label>
                <Input type="number" value={formData.priority} onChange={e => setFormData({...formData, priority: parseInt(e.target.value) || 10})} />
              </div>
              <div className="grid gap-2">
                <Label>Timeout (ms)</Label>
                <Input type="number" value={formData.timeout_ms} onChange={e => setFormData({...formData, timeout_ms: parseInt(e.target.value) || 5000})} />
              </div>
              <div className="grid gap-2">
                <Label>Run As</Label>
                <Select value={formData.run_as} onValueChange={(v) => setFormData({...formData, run_as: v || ''})}>
                  <SelectTrigger><SelectValue placeholder="Trigger User" /></SelectTrigger>
                  <SelectContent>
                    <SelectItem value="">Trigger User</SelectItem>
                    <SelectItem value="system">System</SelectItem>
                    {userEmails.map((email: string) => <SelectItem key={email} value={email}>{email}</SelectItem>)}
                  </SelectContent>
                </Select>
              </div>
            </div>

            {/* Editor */}
            <div className="grid gap-2">
              <div className="flex items-center justify-between">
                <Label>Script <span className="text-xs text-muted-foreground font-mono">(JavaScript)</span></Label>
                <Button type="button" variant="outline" size="sm" onClick={handleValidate} disabled={validating}>
                  {validating ? <Loader2 className="h-3 w-3 mr-1 animate-spin" /> : <FileCheck className="h-3 w-3 mr-1" />}
                  Validate
                </Button>
              </div>
              <CodeEditor
                value={formData.script}
                onChange={(newValue) => setFormData({...formData, script: newValue})}
                minHeight="300px"
              />
            </div>

            {/* Validation result */}
            {validationResult && (
              <div className={`p-3 rounded text-sm ${validationResult.status === 'success' ? 'bg-green-50 text-green-800' : 'bg-red-50 text-red-800'}`}>
                <div className="flex items-center gap-2 font-medium">
                  {validationResult.status === 'success' ? <CheckCircle className="h-4 w-4" /> : <XCircle className="h-4 w-4" />}
                  {validationResult.status === 'success' ? 'Syntax is valid' : 'Syntax error'}
                </div>
                {validationResult.error && <p className="mt-1 font-mono text-xs">{validationResult.error}</p>}
                {validationResult.output && <p className="mt-1 text-xs">{validationResult.output}</p>}
              </div>
            )}

            {formError && (
              <div className="p-3 rounded text-sm bg-red-50 text-red-800 flex items-center gap-2">
                <AlertCircle className="h-4 w-4" /> {formError}
              </div>
            )}

            </div>
            <div className="sticky bottom-0 border-t bg-background px-4 py-3 flex items-center justify-between gap-3">
              <p className="text-xs text-muted-foreground">{editName ? 'Update script configuration and code.' : 'Script will be created as inactive. Activate it after review.'}</p>
              <div className="flex gap-2 shrink-0">
                <Button type="button" variant="outline" size="sm" onClick={() => setDialogOpen(false)}>Cancel</Button>
                <Button type="submit" size="sm" disabled={saving}>
                  {saving && <Loader2 className="h-4 w-4 mr-2 animate-spin" />}
                  {editName ? 'Save Changes' : 'Create Script'}
                </Button>
              </div>
            </div>
          </form>
        </SheetContent>
      </Sheet>

      <ConfirmDialog
        open={confirmDelete !== null}
        onOpenChange={() => setConfirmDelete(null)}
        title="Delete Script"
        description={`Delete script "${confirmDelete}"?`}
        confirmLabel="Delete"
        variant="destructive"
        onConfirm={async () => {
          if (!confirmDelete) return
          await deleteScript(confirmDelete)
          queryClient.invalidateQueries({ queryKey: ['admin', 'scripts'] })
          setConfirmDelete(null)
        }}
      />
    </div>
  )
}
