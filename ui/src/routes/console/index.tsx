import { useState, useEffect } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useConsoleAuthStore } from '@/lib/console-auth-store'
import { listSites, createSite, updateSite, deleteSite, resetSitePassword } from '@/lib/api/console'
import type { ConsoleSite } from '@/types/api'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Badge } from '@/components/ui/badge'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import {
  Sheet, SheetContent, SheetHeader, SheetFooter, SheetTitle, SheetDescription,
} from '@/components/ui/sheet'
import {
  Loader2, Plus, Globe, ChevronDown, ChevronRight, Database, ExternalLink,
  Circle, Search, Server, Activity, Trash2, KeyRound, X, Pencil,
} from 'lucide-react'

function useIsMobile() {
  const [isMobile, setIsMobile] = useState(false)
  useEffect(() => {
    const mq = window.matchMedia('(max-width: 640px)')
    setIsMobile(mq.matches)
    const handler = (e: MediaQueryListEvent) => setIsMobile(e.matches)
    mq.addEventListener('change', handler)
    return () => mq.removeEventListener('change', handler)
  }, [])
  return isMobile
}

function useHealth() {
  return useQuery({
    queryKey: ['console', 'health'],
    queryFn: async () => {
      const resp = await fetch('/health')
      return resp.json() as Promise<{ status: string; db: string; version?: string }>
    },
    staleTime: 15_000,
    refetchInterval: 30_000,
  })
}

function useVersion() {
  return useQuery({
    queryKey: ['console', 'version'],
    queryFn: async () => {
      const resp = await fetch('/api/ping')
      const json = await resp.json() as { message: string; version: string }
      return json
    },
    staleTime: 300_000,
  })
}

export default function ConsoleDashboard() {
  const { needsPasswordChange, token } = useConsoleAuthStore()
  const queryClient = useQueryClient()
  const [search, setSearch] = useState('')
  const [editingSite, setEditingSite] = useState<ConsoleSite | null>(null)
  const [createOpen, setCreateOpen] = useState(false)

  const { data: health } = useHealth()
  const { data: versionInfo } = useVersion()
  const dbConnected = health?.db === 'connected'

  const { data: sites, isLoading, refetch } = useQuery<ConsoleSite[]>({
    queryKey: ['console', 'sites'],
    queryFn: listSites,
    staleTime: 10_000,
  })

  const filteredSites = sites?.filter(s =>
    !search ||
    s.name.toLowerCase().includes(search.toLowerCase()) ||
    s.domains?.some(d => d.toLowerCase().includes(search.toLowerCase()))
  )

  const activeSites = sites?.filter(s => s.status === 'active').length ?? 0
  const errorSites = sites?.filter(s => s.status === 'error').length ?? 0

  if (needsPasswordChange && token) {
    return <ChangePasswordPrompt />
  }

  return (
    <div className="space-y-6">
      {/* Header */}
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">Dashboard</h1>
          <p className="text-sm text-muted-foreground mt-1">Manage your Kora sites</p>
        </div>
        <div className="flex items-center gap-1.5 text-xs">
          <Circle className={`h-2 w-2 ${dbConnected ? 'fill-green-500 text-green-500' : 'fill-muted-foreground/40 text-muted-foreground/40'}`} />
          <span className="text-muted-foreground">
            {!health ? 'Checking...' : health.db === 'connected' ? 'DB Connected' : 'DB Disconnected'}
          </span>
        </div>
      </div>

      {/* Stat cards */}
      <div className="grid grid-cols-2 lg:grid-cols-4 gap-4">
        <Card className="shadow-none">
          <CardContent className="p-4 flex items-center gap-3">
            <div className="h-10 w-10 rounded-lg bg-primary/10 flex items-center justify-center shrink-0">
              <Globe className="h-5 w-5 text-primary" />
            </div>
            <div>
              <p className="text-2xl font-semibold tabular-nums">{sites?.length ?? 0}</p>
              <p className="text-xs text-muted-foreground">Total Sites</p>
            </div>
          </CardContent>
        </Card>
        <Card className="shadow-none">
          <CardContent className="p-4 flex items-center gap-3">
            <div className="h-10 w-10 rounded-lg bg-green-500/10 flex items-center justify-center shrink-0">
              <Activity className="h-5 w-5 text-green-500" />
            </div>
            <div>
              <p className="text-2xl font-semibold tabular-nums">{activeSites}</p>
              <p className="text-xs text-muted-foreground">
                Active{errorSites > 0 ? ` · ${errorSites} error${errorSites > 1 ? 's' : ''}` : ''}
              </p>
            </div>
          </CardContent>
        </Card>
        <Card className="shadow-none">
          <CardContent className="p-4 flex items-center gap-3">
            <div className="h-10 w-10 rounded-lg bg-blue-500/10 flex items-center justify-center shrink-0">
              <Database className="h-5 w-5 text-blue-500" />
            </div>
            <div>
              <p className="text-sm font-semibold">
                {!health ? '...' : health.db === 'connected' ? 'Healthy' : 'Down'}
              </p>
              <p className="text-xs text-muted-foreground">Database</p>
            </div>
          </CardContent>
        </Card>
        <Card className="shadow-none">
          <CardContent className="p-4 flex items-center gap-3">
            <div className="h-10 w-10 rounded-lg bg-muted flex items-center justify-center shrink-0">
              <Server className="h-5 w-5 text-muted-foreground" />
            </div>
            <div>
              <p className="text-sm font-semibold font-mono">{versionInfo?.version ?? '...'}</p>
              <p className="text-xs text-muted-foreground">Kora Version</p>
            </div>
          </CardContent>
        </Card>
      </div>

      {/* Sites table */}
      <Card className="shadow-none">
        <CardHeader className="pb-3">
          <div className="flex items-center justify-between gap-4 flex-wrap">
            <div>
              <CardTitle className="flex items-center gap-2 text-base">
                <Globe className="h-4 w-4" /> Sites
              </CardTitle>
              <CardDescription>{sites?.length ?? 0} site(s) configured</CardDescription>
            </div>
            <div className="flex items-center gap-2">
              <div className="relative">
                <Search className="absolute left-2.5 top-1/2 -translate-y-1/2 h-3.5 w-3.5 text-muted-foreground" />
                <Input
                  placeholder="Search sites..."
                  value={search}
                  onChange={e => setSearch(e.target.value)}
                  className="pl-8 h-8 text-xs w-[180px]"
                />
              </div>
              <Button size="sm" onClick={() => setCreateOpen(true)}>
                <Plus className="h-3.5 w-3.5 mr-1" /> Create Site
              </Button>
            </div>
          </div>
        </CardHeader>
        <CardContent>
          {isLoading ? (
            <div className="space-y-2">
              {[1, 2, 3].map(i => (
                <div key={i} className="h-10 bg-muted/50 rounded animate-pulse" />
              ))}
            </div>
          ) : !filteredSites || filteredSites.length === 0 ? (
            <div className="text-center py-12 text-sm text-muted-foreground border-2 border-dashed rounded-lg">
              {search ? (
                <>No sites match "{search}".</>
              ) : (
                <div className="space-y-2">
                  <Globe className="h-8 w-8 mx-auto text-muted-foreground/40" />
                  <p>No sites yet.</p>
                  <Button variant="outline" size="sm" onClick={() => setCreateOpen(true)}>
                    <Plus className="h-3.5 w-3.5 mr-1" /> Create your first site
                  </Button>
                </div>
              )}
            </div>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Hostname</TableHead>
                  <TableHead>Domains</TableHead>
                  <TableHead className="text-right">DocTypes</TableHead>
                  <TableHead>Status</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {filteredSites.map((s) => (
                  <TableRow
                    key={s.name}
                    className="cursor-pointer hover:bg-muted/50 transition-colors"
                    onClick={() => setEditingSite(s)}
                  >
                    <TableCell className="font-medium">{s.name}</TableCell>
                    <TableCell className="text-xs text-muted-foreground max-w-[200px] truncate">
                      {s.domains?.join(', ') || '-'}
                    </TableCell>
                    <TableCell className="text-right tabular-nums">{s.doctypes}</TableCell>
                    <TableCell>
                      <Badge variant={s.status === 'active' ? 'default' : 'destructive'}>
                        {s.status}
                      </Badge>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>

      {/* Edit Site Sheet */}
      <SiteEditSheet
        site={editingSite}
        onClose={() => setEditingSite(null)}
        onUpdated={() => refetch()}
      />

      {/* Create Site Sheet */}
      <CreateSiteSheet
        open={createOpen}
        onClose={() => setCreateOpen(false)}
        onCreated={() => { refetch(); setCreateOpen(false); }}
      />
    </div>
  )
}

// ────────────────────────────────────────────────────────────────────────────
// Site Edit Sheet
// ────────────────────────────────────────────────────────────────────────────

function SiteEditSheet({
  site,
  onClose,
  onUpdated,
}: {
  site: ConsoleSite | null
  onClose: () => void
  onUpdated: () => void
}) {
  const isMobile = useIsMobile()
  const [domains, setDomains] = useState<string[]>([])
  const [newDomain, setNewDomain] = useState('')
  const [saving, setSaving] = useState(false)
  const [msg, setMsg] = useState<{ text: string; ok: boolean } | null>(null)

  // Reset password state
  const [resetEmail, setResetEmail] = useState('')
  const [resetPassword, setResetPassword] = useState('')
  const [resetting, setResetting] = useState(false)
  const [resetMsg, setResetMsg] = useState<{ text: string; ok: boolean } | null>(null)

  // Delete state
  const [deleteConfirm, setDeleteConfirm] = useState('')
  const [deleting, setDeleting] = useState(false)
  const [deleteMsg, setDeleteMsg] = useState<{ text: string; ok: boolean } | null>(null)

  // Sync state when site changes.
  useEffect(() => {
    if (site) {
      setDomains(site.domains || [])
      setNewDomain('')
      setMsg(null)
      setResetEmail('')
      setResetPassword('')
      setResetMsg(null)
      setDeleteConfirm('')
      setDeleteMsg(null)
    }
  }, [site])

  const addDomain = () => {
    const d = newDomain.trim()
    if (d && !domains.includes(d)) {
      setDomains([...domains, d])
    }
    setNewDomain('')
  }

  const removeDomain = (d: string) => {
    setDomains(domains.filter(x => x !== d))
  }

  const handleSaveDomains = async () => {
    if (!site) return
    setSaving(true)
    setMsg(null)
    try {
      await updateSite(site.name, domains)
      setMsg({ text: 'Domains updated.', ok: true })
      onUpdated()
    } catch (err: any) {
      setMsg({ text: err.message, ok: false })
    }
    setSaving(false)
  }

  const handleResetPassword = async () => {
    if (!site || !resetEmail || !resetPassword) return
    setResetting(true)
    setResetMsg(null)
    try {
      await resetSitePassword(site.name, resetEmail, resetPassword)
      setResetMsg({ text: 'Password reset successfully.', ok: true })
      setResetEmail('')
      setResetPassword('')
    } catch (err: any) {
      setResetMsg({ text: err.message, ok: false })
    }
    setResetting(false)
  }

  const handleDelete = async () => {
    if (!site || deleteConfirm !== site.name) return
    setDeleting(true)
    setDeleteMsg(null)
    try {
      await deleteSite(site.name)
      setDeleteMsg({ text: 'Site deleted.', ok: true })
      setTimeout(() => {
        onClose()
        onUpdated()
      }, 800)
    } catch (err: any) {
      setDeleteMsg({ text: err.message, ok: false })
    }
    setDeleting(false)
  }

  return (
    <Sheet open={!!site} onOpenChange={(open) => { if (!open) onClose() }}>
      <SheetContent side={isMobile ? 'bottom' : 'right'} className="w-full sm:max-w-lg flex flex-col">
        <SheetHeader>
          <SheetTitle>Edit Site: {site?.name}</SheetTitle>
          <SheetDescription>
            Manage domains, admin account, and site actions.
          </SheetDescription>
        </SheetHeader>

        <div className="flex-1 overflow-y-auto space-y-6 py-4">
          {/* Domains */}
          <div className="space-y-3">
            <Label className="text-xs font-medium text-muted-foreground uppercase tracking-wider">Domains</Label>
            <div className="flex flex-wrap gap-1.5">
              {domains.map(d => (
                <span key={d} className="inline-flex items-center gap-1 rounded-md border bg-muted/50 px-2 py-0.5 text-xs">
                  {d}
                  <button
                    onClick={() => removeDomain(d)}
                    className="text-muted-foreground hover:text-foreground transition-colors"
                  >
                    <X className="h-3 w-3" />
                  </button>
                </span>
              ))}
              {domains.length === 0 && (
                <span className="text-xs text-muted-foreground">No domains configured.</span>
              )}
            </div>
            <div className="flex gap-1.5">
              <Input
                placeholder="Add domain..."
                value={newDomain}
                onChange={e => setNewDomain(e.target.value)}
                onKeyDown={e => { if (e.key === 'Enter') addDomain() }}
                className="h-8 text-xs"
              />
              <Button variant="outline" size="sm" onClick={addDomain} className="h-8 text-xs shrink-0">
                Add
              </Button>
            </div>
            {msg && (
              <p className={`text-xs ${msg.ok ? 'text-green-600' : 'text-destructive'}`}>{msg.text}</p>
            )}
            <Button size="sm" onClick={handleSaveDomains} disabled={saving} className="w-full">
              {saving ? <Loader2 className="h-3.5 w-3.5 mr-1 animate-spin" /> : null}
              Save Domains
            </Button>
          </div>

          {/* Reset Password */}
          <div className="space-y-3 border-t pt-6">
            <Label className="text-xs font-medium text-muted-foreground uppercase tracking-wider">Reset Admin Password</Label>
            <div className="space-y-2">
              <Input
                type="email"
                placeholder="User email"
                value={resetEmail}
                onChange={e => setResetEmail(e.target.value)}
                className="h-8 text-xs"
              />
              <Input
                type="password"
                placeholder="New password"
                value={resetPassword}
                onChange={e => setResetPassword(e.target.value)}
                className="h-8 text-xs"
              />
            </div>
            {resetMsg && (
              <p className={`text-xs ${resetMsg.ok ? 'text-green-600' : 'text-destructive'}`}>{resetMsg.text}</p>
            )}
            <Button
              variant="outline"
              size="sm"
              onClick={handleResetPassword}
              disabled={resetting || !resetEmail || !resetPassword}
              className="w-full"
            >
              {resetting ? <Loader2 className="h-3.5 w-3.5 mr-1 animate-spin" /> : <KeyRound className="h-3.5 w-3.5 mr-1" />}
              Reset Password
            </Button>
          </div>

          {/* Danger Zone */}
          <div className="space-y-3 border-t pt-6">
            <Label className="text-xs font-medium text-destructive uppercase tracking-wider">Danger Zone</Label>
            <p className="text-xs text-muted-foreground">
              This will permanently delete the site, its database, and all data. This action cannot be undone.
            </p>
            <Input
              placeholder={`Type "${site?.name}" to confirm`}
              value={deleteConfirm}
              onChange={e => setDeleteConfirm(e.target.value)}
              className="h-8 text-xs"
            />
            {deleteMsg && (
              <p className={`text-xs ${deleteMsg.ok ? 'text-green-600' : 'text-destructive'}`}>{deleteMsg.text}</p>
            )}
            <Button
              variant="destructive"
              size="sm"
              onClick={handleDelete}
              disabled={deleting || deleteConfirm !== site?.name}
              className="w-full"
            >
              {deleting ? <Loader2 className="h-3.5 w-3.5 mr-1 animate-spin" /> : <Trash2 className="h-3.5 w-3.5 mr-1" />}
              Delete This Site
            </Button>
          </div>
        </div>

        <SheetFooter>
          <Button variant="outline" className="w-full" onClick={onClose}>Close</Button>
        </SheetFooter>
      </SheetContent>
    </Sheet>
  )
}

// ────────────────────────────────────────────────────────────────────────────
// Create Site Sheet
// ────────────────────────────────────────────────────────────────────────────

function CreateSiteSheet({
  open,
  onClose,
  onCreated,
}: {
  open: boolean
  onClose: () => void
  onCreated: () => void
}) {
  const isMobile = useIsMobile()
  const [form, setForm] = useState({
    hostname: '', db_type: '', db_host: '', db_port: '',
    db_name: '', db_user: '', db_password: '', domains: '',
    admin_email: '', admin_password: '',
  })
  const [creating, setCreating] = useState(false)
  const [msg, setMsg] = useState<{ text: string; ok: boolean; link?: string } | null>(null)
  const [showAdvanced, setShowAdvanced] = useState(false)

  const derivedDBName = form.hostname.replace(/\./g, '_')
  const effectiveDBName = form.db_name || derivedDBName

  const update = (k: string) => (e: React.ChangeEvent<HTMLInputElement>) =>
    setForm(f => ({ ...f, [k]: e.target.value }))

  const handleCreate = async (e: React.FormEvent) => {
    e.preventDefault()
    setCreating(true)
    setMsg(null)
    try {
      await createSite({
        hostname: form.hostname,
        db_type: form.db_type,
        db_host: form.db_host,
        db_port: parseInt(form.db_port) || 3306,
        db_name: effectiveDBName,
        db_user: form.db_user,
        db_password: form.db_password,
        domains: form.domains || undefined,
        admin_email: form.admin_email,
        admin_password: form.admin_password,
      })
      const workspaceUrl = `/s/${form.hostname}/workspace`
      setMsg({ text: `Site "${form.hostname}" created!`, ok: true, link: workspaceUrl })
      setForm(f => ({ ...f, hostname: '', db_type: '', db_name: '', admin_email: '', admin_password: '', db_password: '' }))
      setTimeout(() => onCreated(), 1000)
    } catch (err: any) {
      setMsg({ text: err.message || 'Failed', ok: false })
    }
    setCreating(false)
  }

  return (
    <Sheet open={open} onOpenChange={(open) => { if (!open) onClose() }}>
      <SheetContent side={isMobile ? 'bottom' : 'right'} className="w-full sm:max-w-lg flex flex-col">
        <SheetHeader>
          <SheetTitle>Create Site</SheetTitle>
          <SheetDescription>
            Only hostname, admin email, and password are required. Database defaults are used unless you provide your own.
          </SheetDescription>
        </SheetHeader>

        <form onSubmit={handleCreate} className="flex-1 overflow-y-auto space-y-4 py-4">
          {msg && (
            <div className={`rounded-lg border px-3 py-2 text-xs ${
              msg.ok ? 'border-green-200 bg-green-50 text-green-800' : 'border-destructive/50 bg-destructive/10 text-destructive'
            }`}>
              {msg.text}
              {msg.link && (
                <a href={msg.link} className="ml-2 underline font-medium hover:no-opacity-80 inline-flex items-center gap-0.5">
                  Open Workspace <ExternalLink className="h-3 w-3" />
                </a>
              )}
            </div>
          )}

          <div className="space-y-3">
            <div className="space-y-1.5">
              <Label htmlFor="hostname">Hostname *</Label>
              <Input id="hostname" placeholder="airtime.local" value={form.hostname} onChange={update('hostname')} required />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="domains">Domains</Label>
              <Input id="domains" placeholder="e.g. kora.sslip.io (comma-separated)" value={form.domains} onChange={update('domains')} />
              <p className="text-[0.8rem] text-muted-foreground">Additional hostnames for this site.</p>
            </div>
            {form.hostname && (
              <div className="text-xs text-muted-foreground flex items-center gap-1.5">
                <Database className="h-3 w-3" />
                Database: <span className="font-mono font-medium text-foreground">{effectiveDBName}</span>
                {form.db_name ? ' (manual)' : ' (auto)'}
              </div>
            )}
          </div>

          <div className="border-t pt-4">
            <p className="text-xs font-medium text-muted-foreground mb-3">Admin Account</p>
            <div className="grid grid-cols-2 gap-3">
              <div className="space-y-1.5">
                <Label htmlFor="admin_email">Admin Email *</Label>
                <Input id="admin_email" type="email" placeholder="admin@airtime.local" value={form.admin_email} onChange={update('admin_email')} required />
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="admin_password">Admin Password *</Label>
                <Input id="admin_password" type="password" value={form.admin_password} onChange={update('admin_password')} required />
              </div>
            </div>
          </div>

          <div className="border-t pt-4">
            <button
              type="button"
              onClick={() => setShowAdvanced(!showAdvanced)}
              className="flex items-center gap-1 text-xs font-medium text-muted-foreground hover:text-foreground transition-colors w-full text-left"
            >
              {showAdvanced ? <ChevronDown className="h-3.5 w-3.5" /> : <ChevronRight className="h-3.5 w-3.5" />}
              Advanced: Self-hosted database (optional)
            </button>
            {showAdvanced && (
              <div className="grid grid-cols-2 gap-3 mt-3">
                <div className="space-y-1.5">
                  <Label htmlFor="db_type">DB Type</Label>
                  <Select value={form.db_type} onValueChange={(v) => setForm(f => ({ ...f, db_type: v ?? '' }))}>
                    <SelectTrigger id="db_type"><SelectValue /></SelectTrigger>
                    <SelectContent>
                      <SelectItem value="mysql">MySQL / MariaDB</SelectItem>
                      <SelectItem value="libsql">LibSQL</SelectItem>
                    </SelectContent>
                  </Select>
                </div>
                <div className="space-y-1.5">
                  <Label htmlFor="db_host">DB Host</Label>
                  <Input id="db_host" placeholder="Default: 127.0.0.1" value={form.db_host} onChange={update('db_host')} />
                </div>
                <div className="space-y-1.5">
                  <Label htmlFor="db_port">DB Port</Label>
                  <Input id="db_port" type="number" placeholder="Default: 3306" value={form.db_port} onChange={update('db_port')} />
                </div>
                <div className="space-y-1.5">
                  <Label htmlFor="db_name">DB Name</Label>
                  <Input id="db_name" placeholder="Auto: derived from hostname" value={form.db_name} onChange={update('db_name')} />
                </div>
                <div className="space-y-1.5">
                  <Label htmlFor="db_user">DB User</Label>
                  <Input id="db_user" placeholder="Default: root" value={form.db_user} onChange={update('db_user')} />
                </div>
                <div className="space-y-1.5 col-span-2">
                  <Label htmlFor="db_password">DB Password</Label>
                  <Input id="db_password" type="password" placeholder="(leave empty if no password)" value={form.db_password} onChange={update('db_password')} />
                </div>
              </div>
            )}
          </div>

          <SheetFooter>
            <div className="flex gap-2 w-full">
              <Button type="button" variant="outline" className="flex-1" onClick={onClose}>Cancel</Button>
              <Button type="submit" disabled={creating} className="flex-1">
                {creating ? <Loader2 className="mr-2 h-4 w-4 animate-spin" /> : null}
                Create Site
              </Button>
            </div>
          </SheetFooter>
        </form>
      </SheetContent>
    </Sheet>
  )
}

// ────────────────────────────────────────────────────────────────────────────
// Change Password Prompt
// ────────────────────────────────────────────────────────────────────────────

function ChangePasswordPrompt() {
  const { changePassword, isLoading, logout } = useConsoleAuthStore()
  const [pw, setPw] = useState('')
  const [msg, setMsg] = useState('')

  const handleChange = async (e: React.FormEvent) => {
    e.preventDefault()
    try {
      await changePassword(pw)
      setMsg('Password changed!')
    } catch (err: any) {
      setMsg(err.message)
    }
  }

  return (
    <Card className="max-w-sm mx-auto mt-12">
      <CardHeader>
        <CardTitle>Change Password</CardTitle>
        <CardDescription>You must change the default password before continuing.</CardDescription>
      </CardHeader>
      <CardContent>
        <form onSubmit={handleChange} className="space-y-4">
          <div className="space-y-2">
            <Label htmlFor="newPassword">New Password</Label>
            <Input id="newPassword" type="password" value={pw} onChange={e => setPw(e.target.value)} required />
          </div>
          {msg && <p className={`text-sm ${msg.includes('changed') ? 'text-green-600' : 'text-destructive'}`}>{msg}</p>}
          <div className="flex gap-2">
            <Button type="submit" disabled={isLoading}>{isLoading ? <Loader2 className="mr-2 h-4 w-4 animate-spin" /> : null}Change</Button>
            <Button type="button" variant="outline" onClick={logout}>Logout</Button>
          </div>
        </form>
      </CardContent>
    </Card>
  )
}
