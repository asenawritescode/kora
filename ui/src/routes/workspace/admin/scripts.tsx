import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useState, useEffect } from 'react'
import {
  fetchScripts, fetchScript, fetchScriptExecutions,
  createScript, updateScript, deleteScript,
} from '@/lib/api/scripts'
import type { ScriptRecord, ScriptExecution } from '@/lib/api/scripts'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Textarea } from '@/components/ui/textarea'
import { Skeleton } from '@/components/ui/skeleton'
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from '@/components/ui/sheet'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Badge } from '@/components/ui/badge'
import { Switch } from '@/components/ui/switch'
import {
  Code2, Plus, Pencil, Trash2, Play, History, AlertCircle,
  Loader2, CheckCircle, XCircle, Clock,
} from 'lucide-react'

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

  const [testResult, setTestResult] = useState<{ status: string; output?: string; error?: string; duration?: number } | null>(null)
  const [testing, setTesting] = useState(false)

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
    if (!confirm(`Delete script "${name}"?`)) return
    await deleteScript(name)
    queryClient.invalidateQueries({ queryKey: ['admin', 'scripts'] })
  }

  const handleToggleActive = async (s: ScriptRecord) => {
    await updateScript(s.name, { is_active: !s.is_active })
    queryClient.invalidateQueries({ queryKey: ['admin', 'scripts'] })
  }

  const handleTest = async () => {
    setTesting(true)
    setTestResult(null)
    try {
      // Simulate test — in production this calls the validate endpoint
      const res = await fetch('/api/system/script/_validate', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ script: formData.script }),
      })
      const data = await res.json()
      if (data.data?.valid) {
        setTestResult({ status: 'success', output: 'Script compiles successfully.', duration: 0 })
      } else {
        setTestResult({ status: 'error', error: data.data?.error || 'Unknown error' })
      }
    } catch (err: any) {
      setTestResult({ status: 'error', error: err?.message || 'Test failed' })
    } finally {
      setTesting(false)
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
      {executionsFor && (
        <Card>
          <CardHeader>
            <CardTitle className="text-lg flex items-center gap-2">
              <History className="h-5 w-5" /> Executions: {executionsFor}
            </CardTitle>
            <CardDescription>Recent script execution log.</CardDescription>
          </CardHeader>
          <CardContent>
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
                {executions?.map((e) => (
                  <TableRow key={e.id}>
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
                    <TableCell className="text-xs text-red-500 max-w-[200px] truncate">{e.error_message || ''}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </CardContent>
        </Card>
      )}

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
                  <Input value={formData.doctype || ''} onChange={e => setFormData({...formData, doctype: e.target.value})} placeholder="e.g., Work Order" />
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
                <Input value={formData.run_as} onChange={e => setFormData({...formData, run_as: e.target.value})}
                  placeholder="Trigger User" />
              </div>
            </div>

            {/* Editor */}
            <div className="grid gap-2">
              <div className="flex items-center justify-between">
                <Label>Script <span className="text-xs text-muted-foreground font-mono">(JavaScript)</span></Label>
                <Button type="button" variant="outline" size="sm" onClick={handleTest} disabled={testing}>
                  {testing ? <Loader2 className="h-3 w-3 mr-1 animate-spin" /> : <Play className="h-3 w-3 mr-1" />}
                  Test
                </Button>
              </div>
              <Textarea
                value={formData.script}
                onChange={e => setFormData({...formData, script: e.target.value})}
                className="font-mono text-sm min-h-[300px] bg-zinc-950 text-zinc-100"
                placeholder="// Write your JavaScript here"
                rows={16}
              />
            </div>

            {/* Test result */}
            {testResult && (
              <div className={`p-3 rounded text-sm ${testResult.status === 'success' ? 'bg-green-50 text-green-800' : 'bg-red-50 text-red-800'}`}>
                <div className="flex items-center gap-2 font-medium">
                  {testResult.status === 'success' ? <CheckCircle className="h-4 w-4" /> : <XCircle className="h-4 w-4" />}
                  {testResult.status === 'success' ? 'Script compiles successfully' : 'Script error'}
                </div>
                {testResult.error && <p className="mt-1 font-mono text-xs">{testResult.error}</p>}
                {testResult.output && <p className="mt-1 text-xs">{testResult.output}</p>}
              </div>
            )}

            {formError && (
              <div className="p-3 rounded text-sm bg-red-50 text-red-800 flex items-center gap-2">
                <AlertCircle className="h-4 w-4" /> {formError}
              </div>
            )}

            </div>
            <div className="sticky bottom-0 border-t bg-background px-4 py-3 flex items-center justify-between gap-3">
              <p className="text-xs text-muted-foreground">{editName ? 'Update script configuration.' : 'Script will be created as inactive. Activate it after review.'}</p>
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
    </div>
  )
}
