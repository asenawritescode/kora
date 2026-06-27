import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useState, useEffect, useMemo } from 'react'
import { fetchExtensions, createExtension, deleteExtension, fetchDeliveries, rotateSecret } from '@/lib/api/extensions'
import { fetchDoctypes, fetchDoctypeSchema } from '@/lib/api/system'
import type { ExtensionRecord, DeliveryRecord } from '@/lib/api/extensions'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Skeleton } from '@/components/ui/skeleton'
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from '@/components/ui/sheet'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Badge } from '@/components/ui/badge'
import { Webhook, Plus, Trash2, History, KeyRound, Loader2, ChevronDown, X } from 'lucide-react'
import { toast } from '@/components/ui/Toast'
import { ConfirmDialog } from '@/components/ui/confirm-dialog'
import { cn } from '@/lib/utils'

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

export default function AdminExtensionsPage() {
  const isMobile = useIsMobile()
  const queryClient = useQueryClient()
  const { data: extensions, isLoading } = useQuery({
    queryKey: ['admin', 'extensions'],
    queryFn: fetchExtensions,
  })

  const [dialogOpen, setDialogOpen] = useState(false)
  const [form, setForm] = useState({ name: '', display_name: '', description: '', endpoint_url: '', subscriptions: '[]', api_permissions: '[]' })
  const [saving, setSaving] = useState(false)
  const [newSecret, setNewSecret] = useState<string | null>(null)
  type ConfirmAction = { type: 'delete'; name: string } | { type: 'rotate'; name: string } | null
  const [confirmAction, setConfirmAction] = useState<ConfirmAction>(null)

  const [deliveriesFor, setDeliveriesFor] = useState<string | null>(null)
  const { data: deliveries } = useQuery({
    queryKey: ['admin', 'extensions', deliveriesFor, 'deliveries'],
    queryFn: () => fetchDeliveries(deliveriesFor!),
    enabled: !!deliveriesFor,
  })

  // Data for structured builders.
  const { data: doctypes } = useQuery({
    queryKey: ['admin', 'doctypes'],
    queryFn: fetchDoctypes,
  })
  const doctypeNames = (doctypes as any[])?.map((d: any) => d.name) || []

  const EVENTS = ['after_insert', 'after_save', 'after_delete', 'after_submit', 'after_cancel']
  const OPS = ['read', 'write', 'create', 'delete', 'submit', 'cancel', 'amend', 'export', 'import', 'report']

  // Parsed builder state (synced to JSON strings on save).
  type ParsedSub = { doctype: string; event: string }
  type ParsedPerm = { doctype: string; operations: string[] }
  const [subs, setSubs] = useState<ParsedSub[]>([])
  const [perms, setPerms] = useState<ParsedPerm[]>([])

  // Parse JSON into structured state when opening the sheet.
  const initBuilderState = (subsJson: string, permsJson: string) => {
    try { setSubs(JSON.parse(subsJson || '[]').map((s: any) => ({
      doctype: (s.event || '').replace('kora.', '').split('.')[0] || '',
      event: (s.event || '').split('.').pop() || '',
    }))) } catch { setSubs([]) }
    try { setPerms(parsePerms(permsJson)) } catch { setPerms([]) }
  }

  // Serialize builder state back to JSON strings.
  const serializeSubs = () => JSON.stringify(subs.filter(s => s.doctype && s.event).map(s => ({
    event: `kora.${s.doctype}.${s.event}`,
  })))
  const allOps = ['read', 'write', 'create', 'delete', 'submit', 'cancel', 'amend', 'export', 'import', 'report'] as const
  const serializePerms = () => {
    const validPerms = perms.filter(p => p.doctype && p.operations.length > 0)
    return JSON.stringify(validPerms.map(p => {
      const ops = new Set(p.operations)
      return {
        doctype: p.doctype,
        read: ops.has('read'),
        write: ops.has('write'),
        create: ops.has('create'),
        delete: ops.has('delete'),
        submit: ops.has('submit'),
        cancel: ops.has('cancel'),
        amend: ops.has('amend'),
        export: ops.has('export'),
        import: ops.has('import'),
        report: ops.has('report'),
      }
    }))
  }
  // Parse both boolean-flag format (stored) and operations-array format (builder).
  const parsePerms = (json: string) => {
    try {
      const parsed = JSON.parse(json || '[]')
      return parsed.map((p: any) => ({
        doctype: p.doctype || '',
        operations: Array.isArray(p.operations)
          ? p.operations
          : allOps.filter(op => p[op] === true),
      }))
    } catch { return [] }
  }

  const handleCreate = async (e: React.FormEvent) => {
    e.preventDefault()
    setSaving(true)
    try {
      const result = await createExtension({
        name: form.name, display_name: form.display_name, description: form.description,
        endpoint_url: form.endpoint_url, subscriptions: serializeSubs(), api_permissions: serializePerms(),
      })
      setNewSecret(result.secret)
      queryClient.invalidateQueries({ queryKey: ['admin', 'extensions'] })
    } catch (err: any) {
      toast('error', err?.message || 'Failed to create extension')
    } finally {
      setSaving(false)
    }
  }

  const handleDelete = async (name: string) => {
    setConfirmAction({ type: 'delete', name })
  }

  const handleRotate = async (name: string) => {
    setConfirmAction({ type: 'rotate', name })
  }

  const openCreate = () => {
    setForm({ name: '', display_name: '', description: '', endpoint_url: '', subscriptions: '[]', api_permissions: '[]' })
    setNewSecret(null)
    initBuilderState('[]', '[]')
    setDialogOpen(true)
  }

  if (isLoading) return <div className="p-6 space-y-4"><Skeleton className="h-8 w-64" /><Skeleton className="h-64 w-full" /></div>

  return (
    <div className="p-6 space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-bold tracking-tight flex items-center gap-2">
            <Webhook className="h-6 w-6" /> Extensions
          </h1>
          <p className="text-muted-foreground mt-1">Webhook-based extensions that receive Kora events.</p>
        </div>
        <Button onClick={openCreate}><Plus className="h-4 w-4 mr-2" /> Register Extension</Button>
      </div>

      {/* Extension list — desktop table */}
      <Card>
        <CardContent className="p-0 hidden md:block">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead>Endpoint</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>Deliveries</TableHead>
                <TableHead className="w-[120px]">Actions</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {(!extensions || extensions.length === 0) && (
                <TableRow><TableCell colSpan={5} className="text-center text-muted-foreground py-8">
                  No extensions yet. Register one to start receiving events.
                </TableCell></TableRow>
              )}
              {extensions?.map((ext) => (
                <TableRow key={ext.name}>
                  <TableCell className="font-medium">
                    {ext.display_name || ext.name}
                    {ext.consecutive_failures > 0 && (
                      <Badge variant="destructive" className="ml-2 text-xs">{ext.consecutive_failures} failures</Badge>
                    )}
                  </TableCell>
                  <TableCell className="text-sm text-muted-foreground max-w-[200px] truncate" title={ext.endpoint_url}>
                    {ext.endpoint_url}
                  </TableCell>
                  <TableCell>
                    <Badge variant={ext.is_active ? 'default' : 'secondary'}>
                      {ext.is_active ? 'Active' : 'Disabled'}
                    </Badge>
                  </TableCell>
                  <TableCell className="text-sm text-muted-foreground">
                    {ext.last_delivery_at ? new Date(ext.last_delivery_at).toLocaleDateString() : 'Never'}
                  </TableCell>
                  <TableCell>
                    <div className="flex gap-1">
                      <Button variant="ghost" size="icon" title="Deliveries" onClick={() => setDeliveriesFor(deliveriesFor === ext.name ? null : ext.name)}>
                        <History className="h-4 w-4" />
                      </Button>
                      <Button variant="ghost" size="icon" title="Rotate Secret" onClick={() => handleRotate(ext.name)}>
                        <KeyRound className="h-4 w-4" />
                      </Button>
                      <Button variant="ghost" size="icon" title="Delete" onClick={() => handleDelete(ext.name)}>
                        <Trash2 className="h-4 w-4 text-red-500" />
                      </Button>
                    </div>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </CardContent>

        {/* Extension list — mobile stacked cards */}
        <CardContent className="md:hidden p-0">
        <div className="divide-y">
          {(!extensions || extensions.length === 0) && (
            <div className="text-center text-muted-foreground py-8 text-sm">No extensions yet. Register one to start receiving events.</div>
          )}
          {extensions?.map((ext) => (
            <div key={ext.name} className="p-4 space-y-2">
              <div className="flex items-center justify-between">
                <div className="flex items-center gap-2">
                  <span className="font-medium text-sm">{ext.display_name || ext.name}</span>
                  <Badge variant={ext.is_active ? 'default' : 'secondary'} className="text-xs">
                    {ext.is_active ? 'Active' : 'Disabled'}
                  </Badge>
                </div>
                {ext.consecutive_failures > 0 && (
                  <Badge variant="destructive" className="text-xs">{ext.consecutive_failures} failures</Badge>
                )}
              </div>
              <div className="text-xs text-muted-foreground truncate">{ext.endpoint_url}</div>
              <div className="text-xs text-muted-foreground">
                Last delivery: {ext.last_delivery_at ? new Date(ext.last_delivery_at).toLocaleDateString() : 'Never'}
              </div>
              <div className="flex gap-1 justify-end">
                <Button variant="ghost" size="sm" onClick={() => setDeliveriesFor(deliveriesFor === ext.name ? null : ext.name)}><History className="h-3.5 w-3.5 mr-1" />Log</Button>
                <Button variant="ghost" size="sm" onClick={() => handleRotate(ext.name)}><KeyRound className="h-3.5 w-3.5 mr-1" />Rotate</Button>
                <Button variant="ghost" size="sm" onClick={() => handleDelete(ext.name)}><Trash2 className="h-3.5 w-3.5 mr-1 text-red-500" />Delete</Button>
              </div>
            </div>
          ))}
        </div>
        </CardContent>
      </Card>

      {/* Delivery log */}
      {deliveriesFor && (
        <Card>
          <CardHeader>
            <CardTitle className="text-lg flex items-center gap-2">
              <History className="h-5 w-5" /> Deliveries: {deliveriesFor}
            </CardTitle>
          </CardHeader>
          <CardContent>
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Time</TableHead>
                  <TableHead>Event</TableHead>
                  <TableHead>Status</TableHead>
                  <TableHead>Attempt</TableHead>
                  <TableHead>Duration</TableHead>
                  <TableHead>Error</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {(!deliveries || deliveries.length === 0) && (
                  <TableRow><TableCell colSpan={6} className="text-center text-muted-foreground py-4">No deliveries yet.</TableCell></TableRow>
                )}
                {deliveries?.map((d) => (
                  <TableRow key={d.id}>
                    <TableCell className="text-xs text-muted-foreground">{new Date(d.created_at).toLocaleString()}</TableCell>
                    <TableCell className="text-xs font-mono max-w-[200px] truncate">{d.event_type}</TableCell>
                    <TableCell>
                      <Badge variant={d.status === 'delivered' ? 'default' : d.status === 'dead_lettered' ? 'destructive' : 'secondary'}>
                        {d.status}
                      </Badge>
                    </TableCell>
                    <TableCell className="text-xs">{d.attempt}</TableCell>
                    <TableCell className="text-xs">{d.duration_ms}ms</TableCell>
                    <TableCell className="text-xs text-red-500 max-w-[200px] truncate">{d.error_message || d.response_status ? `HTTP ${d.response_status}` : ''}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </CardContent>
        </Card>
      )}

      {/* Create Sheet */}
      <Sheet open={dialogOpen} onOpenChange={(open) => { if (!open) { setDialogOpen(false); setNewSecret(null) } }}>
        <SheetContent side={isMobile ? 'bottom' : 'right'} className={isMobile ? 'w-full max-h-[85vh] rounded-t-xl flex flex-col' : 'w-full sm:max-w-xl flex flex-col'}>
          <SheetHeader className="border-b pb-4">
            <SheetTitle className="text-lg">Register Extension</SheetTitle>
            <SheetDescription>Extensions receive webhooks when events occur in Kora. Create one to start receiving events.</SheetDescription>
          </SheetHeader>

          {newSecret ? (
            <div className="flex-1 overflow-y-auto p-4 space-y-6">
              <div className="p-4 rounded-lg bg-amber-50 border border-amber-200 space-y-3">
                <p className="font-semibold text-amber-800 text-base">Extension created!</p>
                <p className="text-sm text-amber-700">This is your signing secret. Store it securely — it will not be shown again.</p>
                <div className="flex items-center gap-2">
                  <Input value={newSecret} readOnly className="font-mono text-sm bg-white" />
                  <Button variant="outline" size="sm" onClick={() => navigator.clipboard.writeText(newSecret)}>Copy</Button>
                </div>
              </div>
            </div>
          ) : (
            <form onSubmit={handleCreate} className="flex-1 flex flex-col min-h-0">
              <div className="flex-1 overflow-y-auto p-4 space-y-5">
                <div className="space-y-1.5">
                  <Label htmlFor="ext-name" className="text-sm font-medium">Name <span className="text-red-500">*</span></Label>
                  <Input id="ext-name" value={form.name} onChange={e => setForm({...form, name: e.target.value})} placeholder="e.g., my-slack-bot" required />
                  <p className="text-[11px] text-muted-foreground">A unique identifier for this extension.</p>
                </div>
                <div className="space-y-1.5">
                  <Label htmlFor="ext-dn" className="text-sm font-medium">Display Name</Label>
                  <Input id="ext-dn" value={form.display_name} onChange={e => setForm({...form, display_name: e.target.value})} placeholder="e.g., Slack Notifications" />
                </div>
                <div className="space-y-1.5">
                  <Label htmlFor="ext-url" className="text-sm font-medium">Endpoint URL <span className="text-red-500">*</span></Label>
                  <Input id="ext-url" type="url" value={form.endpoint_url} onChange={e => setForm({...form, endpoint_url: e.target.value})} placeholder="https://my-worker.workers.dev/webhook" required />
                  <p className="text-[11px] text-muted-foreground">Kora will POST event payloads to this URL.</p>
                </div>
                {/* Event Subscriptions — structured builder */}
                <div className="space-y-2">
                  <div className="flex items-center justify-between">
                    <Label className="text-sm font-medium">Event Subscriptions</Label>
                    <Button type="button" variant="ghost" size="sm" onClick={() => setSubs([...subs, { doctype: '', event: 'after_save' }])}>
                      <Plus className="h-3 w-3 mr-1" /> Add
                    </Button>
                  </div>
                  <p className="text-[11px] text-muted-foreground">Choose which Kora events trigger this webhook.</p>
                  {subs.length === 0 && (
                    <p className="text-xs text-muted-foreground italic py-2">No subscriptions yet. Click Add to subscribe to an event.</p>
                  )}
                  {subs.map((s, i) => (
                    <div key={i} className="flex items-center gap-2 p-2 border rounded-md bg-muted/20">
                      <Select value={s.doctype} onValueChange={(v) => { const n = [...subs]; n[i] = {...n[i], doctype: v || ''}; setSubs(n) }}>
                        <SelectTrigger className="flex-1 h-8 text-xs"><SelectValue placeholder="Doctype..." /></SelectTrigger>
                        <SelectContent>
                          {doctypeNames.map((n: string) => <SelectItem key={n} value={n}>{n}</SelectItem>)}
                        </SelectContent>
                      </Select>
                      <Select value={s.event} onValueChange={(v) => { const n = [...subs]; n[i] = {...n[i], event: v || ''}; setSubs(n) }}>
                        <SelectTrigger className="w-[140px] h-8 text-xs"><SelectValue placeholder="Event..." /></SelectTrigger>
                        <SelectContent>
                          {EVENTS.map(ev => <SelectItem key={ev} value={ev}>{ev.replace(/_/g, ' ')}</SelectItem>)}
                        </SelectContent>
                      </Select>
                      <Button variant="ghost" size="icon" className="h-7 w-7 shrink-0" onClick={() => setSubs(subs.filter((_, j) => j !== i))}>
                        <X className="h-3.5 w-3.5 text-muted-foreground" />
                      </Button>
                    </div>
                  ))}
                  <details className="text-[11px] text-muted-foreground">
                    <summary className="cursor-pointer">Edit as JSON</summary>
                    <textarea value={serializeSubs()} onChange={(e) => { try { setSubs(JSON.parse(e.target.value).map((s: any) => ({ doctype: s.event.replace('kora.', '').split('.')[0] || '', event: s.event.split('.').pop() || '' }))) } catch {} }}
                      className="w-full font-mono text-xs bg-zinc-950 text-zinc-100 border border-zinc-700 rounded p-2 mt-1" rows={4} />
                  </details>
                </div>

                {/* API Permissions — structured builder */}
                <div className="space-y-2">
                  <div className="flex items-center justify-between">
                    <Label className="text-sm font-medium">API Access</Label>
                    <Button type="button" variant="ghost" size="sm" onClick={() => setPerms([...perms, { doctype: '', operations: ['read'] }])}>
                      <Plus className="h-3 w-3 mr-1" /> Add
                    </Button>
                  </div>
                  <p className="text-[11px] text-muted-foreground">Grant this extension permission to read or modify data via the Kora API.</p>
                  <p className="text-[11px] text-amber-600">Permissions are enforced on every API call. An extension with no permissions cannot access any data.</p>
                  {perms.length === 0 && (
                    <p className="text-xs text-muted-foreground italic py-2">No permissions yet. Click Add to grant API access.</p>
                  )}
                  {perms.map((p, i) => (
                    <div key={i} className="p-2 border rounded-md bg-muted/20 space-y-2">
                      <div className="flex items-center gap-2">
                        <Select value={p.doctype} onValueChange={(v) => { const n = [...perms]; n[i] = {...n[i], doctype: v || ''}; setPerms(n) }}>
                          <SelectTrigger className="flex-1 h-8 text-xs"><SelectValue placeholder="Doctype..." /></SelectTrigger>
                          <SelectContent>
                            {doctypeNames.map((n: string) => <SelectItem key={n} value={n}>{n}</SelectItem>)}
                          </SelectContent>
                        </Select>
                        <Button variant="ghost" size="icon" className="h-7 w-7 shrink-0" onClick={() => setPerms(perms.filter((_, j) => j !== i))}>
                          <X className="h-3.5 w-3.5 text-muted-foreground" />
                        </Button>
                      </div>
                      <div className="flex flex-wrap gap-1">
                        {OPS.map(op => {
                          const active = p.operations.includes(op)
                          return (
                            <button key={op} type="button" onClick={() => {
                              const n = [...perms]
                              n[i] = {...n[i], operations: active ? n[i].operations.filter(o => o !== op) : [...n[i].operations, op]}
                              setPerms(n)
                            }} className={cn(
                              'inline-flex items-center rounded-full border px-2 py-0.5 text-[11px] font-medium transition-colors',
                              active ? 'bg-primary border-primary text-primary-foreground' : 'border-input bg-background text-muted-foreground hover:border-primary/50'
                            )}>{op}</button>
                          )
                        })}
                      </div>
                    </div>
                  ))}
                  <details className="text-[11px] text-muted-foreground">
                    <summary className="cursor-pointer">Edit as JSON</summary>
                    <textarea value={serializePerms()} onChange={(e) => { try { setPerms(parsePerms(e.target.value)) } catch {} }}
                      className="w-full font-mono text-xs bg-zinc-950 text-zinc-100 border border-zinc-700 rounded p-2 mt-1" rows={4} />
                  </details>
                </div>
              </div>
              <div className="sticky bottom-0 border-t bg-background px-4 py-3 flex items-center justify-between gap-3">
                <p className="text-xs text-muted-foreground">A signing secret will be generated on registration.</p>
                <div className="flex gap-2 shrink-0">
                  <Button type="button" variant="outline" size="sm" onClick={() => setDialogOpen(false)}>Cancel</Button>
                  <Button type="submit" size="sm" disabled={saving}>
                    {saving && <Loader2 className="h-4 w-4 mr-2 animate-spin" />}
                    Register
                  </Button>
                </div>
              </div>
            </form>
          )}
        </SheetContent>
      </Sheet>

      <ConfirmDialog
        open={confirmAction !== null}
        onOpenChange={() => setConfirmAction(null)}
        title={confirmAction?.type === 'delete' ? 'Delete Extension' : 'Rotate Secret'}
        description={
          confirmAction?.type === 'delete'
            ? `Delete extension "${confirmAction?.name}"? This cannot be undone.`
            : `Rotate secret for "${confirmAction?.name}"? The old secret will work for 24 hours.`
        }
        confirmLabel={confirmAction?.type === 'delete' ? 'Delete' : 'Rotate'}
        variant={confirmAction?.type === 'delete' ? 'destructive' : 'default'}
        onConfirm={async () => {
          if (!confirmAction) return
          if (confirmAction.type === 'delete') {
            await deleteExtension(confirmAction.name)
            queryClient.invalidateQueries({ queryKey: ['admin', 'extensions'] })
          } else {
            const result = await rotateSecret(confirmAction.name)
            setNewSecret(result.secret)
          }
          setConfirmAction(null)
        }}
      />
    </div>
  )
}
