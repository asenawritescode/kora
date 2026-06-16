import { useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useConsoleAuthStore } from '@/lib/console-auth-store'
import { listSites, createSite } from '@/lib/api/console'
import type { ConsoleSite } from '@/types/api'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Badge } from '@/components/ui/badge'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Loader2, Plus, Globe, ChevronDown, ChevronRight, Database, ExternalLink, Circle } from 'lucide-react'

function useHealth() {
  return useQuery({
    queryKey: ['console', 'health'],
    queryFn: async () => {
      const resp = await fetch('/health')
      return resp.json() as Promise<{ status: string; db: string }>
    },
    staleTime: 15_000,
    refetchInterval: 30_000,
  })
}

export default function ConsoleDashboard() {
  const { needsPasswordChange, token } = useConsoleAuthStore()
  const queryClient = useQueryClient()
  const [form, setForm] = useState({
    hostname: '', db_type: '', db_host: '', db_port: '',
    db_name: '', db_user: '', db_password: '', admin_email: '', admin_password: '',
  })
  const [creating, setCreating] = useState(false)
  const [msg, setMsg] = useState<{ text: string; ok: boolean; link?: string } | null>(null)
  const [showAdvanced, setShowAdvanced] = useState(false)
  const [createdSite, setCreatedSite] = useState<string | null>(null)

  const { data: health } = useHealth()
  const dbConnected = health?.db === 'connected'
  const dbStatus = !health ? '...' : health.db

  // Auto-derive DB name from hostname (same as old hostnameToDBName in CLI).
  const derivedDBName = form.hostname.replace(/\./g, '_')
  const effectiveDBName = form.db_name || derivedDBName

  const { data: sites, isLoading, refetch } = useQuery<ConsoleSite[]>({
    queryKey: ['console', 'sites'],
    queryFn: listSites,
    staleTime: 10_000,
  })

  const update = (k: string) => (e: React.ChangeEvent<HTMLInputElement>) =>
    setForm(f => ({ ...f, [k]: e.target.value }))

  const handleCreate = async (e: React.FormEvent) => {
    e.preventDefault()
    setCreating(true)
    setMsg(null)
    setCreatedSite(null)
    try {
      await createSite({
        hostname: form.hostname,
        db_type: form.db_type,
        db_host: form.db_host,
        db_port: parseInt(form.db_port) || 3306,
        db_name: effectiveDBName,
        db_user: form.db_user,
        db_password: form.db_password,
        admin_email: form.admin_email,
        admin_password: form.admin_password,
      })
      const workspaceUrl = `/s/${form.hostname}/workspace`
      setMsg({ text: `Site "${form.hostname}" created! `, ok: true, link: workspaceUrl })
      setCreatedSite(form.hostname)
      setForm(f => ({ ...f, hostname: '', db_type: '', db_name: '', admin_email: '', admin_password: '', db_password: '' }))
      queryClient.invalidateQueries({ queryKey: ['console', 'sites'] })
      refetch()
    } catch (err: any) {
      setMsg({ text: err.message || 'Failed', ok: false })
    }
    setCreating(false)
  }

  if (needsPasswordChange && token) {
    return <ChangePasswordPrompt />
  }

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">Dashboard</h1>
          <p className="text-sm text-muted-foreground mt-1">Manage your Kora sites</p>
        </div>
        <div className="flex items-center gap-1.5 text-xs">
          <Circle className={`h-2 w-2 ${dbConnected ? 'fill-green-500 text-green-500' : 'fill-muted-foreground/40 text-muted-foreground/40'}`} />
          <span className="text-muted-foreground">
            Database: {dbStatus === 'connected' ? 'Connected' : dbStatus === 'disconnected' ? 'Disconnected' : dbStatus === '...' ? 'Checking...' : 'Not configured'}
          </span>
        </div>
      </div>

      {msg && (
        <div className={`rounded-lg border px-4 py-3 text-sm ${
          msg.ok ? 'border-green-200 bg-green-50 text-green-800' : 'border-destructive/50 bg-destructive/10 text-destructive'
        }`}>
          {msg.text}
          {msg.link && (
            <a href={msg.link} className="ml-2 underline font-medium hover:no-underline inline-flex items-center gap-0.5">
              Open Workspace <ExternalLink className="h-3 w-3" />
            </a>
          )}
        </div>
      )}

      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2"><Globe className="h-4 w-4" /> Sites</CardTitle>
          <CardDescription>{sites?.length || 0} site(s) configured</CardDescription>
        </CardHeader>
        <CardContent>
          {isLoading ? (
            <div className="flex items-center gap-2 text-sm text-muted-foreground py-8 justify-center">
              <Loader2 className="h-4 w-4 animate-spin" /> Loading...
            </div>
          ) : !sites || sites.length === 0 ? (
            <div className="text-center py-8 text-sm text-muted-foreground border-2 border-dashed rounded-lg">
              No sites yet. Create your first site below.
            </div>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Hostname</TableHead>
                  <TableHead>Domains</TableHead>
                  <TableHead>DocTypes</TableHead>
                  <TableHead>Status</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {sites.map((s) => (
                  <TableRow key={s.name}>
                    <TableCell className="font-medium">{s.name}</TableCell>
                    <TableCell className="text-xs text-muted-foreground">{s.domains?.join(', ') || '-'}</TableCell>
                    <TableCell>{s.doctypes}</TableCell>
                    <TableCell>
                      <Badge variant={s.status === 'active' ? 'default' : 'destructive'}>{s.status}</Badge>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2"><Plus className="h-4 w-4" /> Create Site</CardTitle>
          <CardDescription>
            Only hostname, admin email, and admin password are required. Database defaults are used unless you provide your own.
          </CardDescription>
        </CardHeader>
        <CardContent>
          <form onSubmit={handleCreate} className="space-y-4">
            {/* Required fields */}
            <div className="space-y-4">
              <div className="space-y-2">
                <Label htmlFor="hostname">Hostname *</Label>
                <Input id="hostname" placeholder="airtime.local" value={form.hostname} onChange={update('hostname')} required />
              </div>
              {form.hostname && (
                <div className="text-xs text-muted-foreground flex items-center gap-1.5 pl-1">
                  <Database className="h-3 w-3" />
                  Database: <span className="font-mono font-medium text-foreground">{effectiveDBName}</span>
                  {form.db_name ? ' (manual)' : ' (auto)'}
                </div>
              )}
            </div>

            {/* Admin account */}
            <div className="border-t pt-4">
              <p className="text-xs font-medium text-muted-foreground mb-3">Admin Account</p>
              <div className="grid grid-cols-2 gap-4">
                <div className="space-y-2">
                  <Label htmlFor="admin_email">Admin Email *</Label>
                  <Input id="admin_email" type="email" placeholder="admin@airtime.local" value={form.admin_email} onChange={update('admin_email')} required />
                </div>
                <div className="space-y-2">
                  <Label htmlFor="admin_password">Admin Password *</Label>
                  <Input id="admin_password" type="password" value={form.admin_password} onChange={update('admin_password')} required />
                </div>
              </div>
            </div>

            {/* Advanced: Self-hosted database (collapsible) */}
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
                <div className="grid grid-cols-2 gap-4 mt-3">
                  <div className="space-y-2">
                    <Label htmlFor="db_type">DB Type</Label>
                    <Select value={form.db_type} onValueChange={(v) => setForm(f => ({ ...f, db_type: v ?? '' }))}>
                      <SelectTrigger id="db_type"><SelectValue /></SelectTrigger>
                      <SelectContent>
                        <SelectItem value="mysql">MySQL / MariaDB</SelectItem>
                        <SelectItem value="libsql">LibSQL</SelectItem>
                      </SelectContent>
                    </Select>
                  </div>
                  <div className="space-y-2">
                    <Label htmlFor="db_host">DB Host</Label>
                    <Input id="db_host" placeholder="Default: 127.0.0.1" value={form.db_host} onChange={update('db_host')} />
                  </div>
                  <div className="space-y-2">
                    <Label htmlFor="db_port">DB Port</Label>
                    <Input id="db_port" type="number" placeholder="Default: 3306" value={form.db_port} onChange={update('db_port')} />
                  </div>
                  <div className="space-y-2">
                    <Label htmlFor="db_name">DB Name</Label>
                    <Input id="db_name" placeholder="Auto: derived from hostname" value={form.db_name} onChange={update('db_name')} />
                  </div>
                  <div className="space-y-2">
                    <Label htmlFor="db_user">DB User</Label>
                    <Input id="db_user" placeholder="Default: root" value={form.db_user} onChange={update('db_user')} />
                  </div>
                  <div className="space-y-2 col-span-2">
                    <Label htmlFor="db_password">DB Password</Label>
                    <Input id="db_password" type="password" placeholder="(leave empty if no password)" value={form.db_password} onChange={update('db_password')} />
                  </div>
                </div>
              )}
            </div>

            <Button type="submit" disabled={creating}>
              {creating ? <Loader2 className="mr-2 h-4 w-4 animate-spin" /> : null}
              Create Site
            </Button>
          </form>
        </CardContent>
      </Card>
    </div>
  )
}

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
