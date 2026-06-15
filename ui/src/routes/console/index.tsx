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
import { Loader2, Plus, Globe, Server, Database } from 'lucide-react'

export default function ConsoleDashboard() {
  const { needsPasswordChange, token } = useConsoleAuthStore()
  const queryClient = useQueryClient()
  const [form, setForm] = useState({ hostname: '', db_host: 'localhost', db_port: '3306', db_name: '', db_user: 'root', db_password: '', admin_email: '', admin_password: '' })
  const [creating, setCreating] = useState(false)
  const [msg, setMsg] = useState<{ text: string; ok: boolean } | null>(null)

  const { data: sites, isLoading, refetch } = useQuery<ConsoleSite[]>({
    queryKey: ['console', 'sites'],
    queryFn: listSites,
    staleTime: 10_000,
  })

  const update = (k: string) => (e: React.ChangeEvent<HTMLInputElement>) => setForm(f => ({ ...f, [k]: e.target.value }))

  const handleCreate = async (e: React.FormEvent) => {
    e.preventDefault()
    setCreating(true)
    setMsg(null)
    try {
      await createSite({
        hostname: form.hostname, db_host: form.db_host, db_port: parseInt(form.db_port) || 3306,
        db_name: form.db_name, db_user: form.db_user, db_password: form.db_password,
        admin_email: form.admin_email, admin_password: form.admin_password,
      })
      setMsg({ text: `Site "${form.hostname}" created!`, ok: true })
      setForm(f => ({ ...f, hostname: '', db_name: '', admin_email: '', admin_password: '', db_password: '' }))
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
      </div>

      {msg && (
        <div className={`rounded-lg border px-4 py-3 text-sm ${msg.ok ? 'border-green-200 bg-green-50 text-green-800' : 'border-destructive/50 bg-destructive/10 text-destructive'}`}>
          {msg.text}
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
          <CardDescription>Creates database, bootstraps system tables, and sets up admin user</CardDescription>
        </CardHeader>
        <CardContent>
          <form onSubmit={handleCreate} className="space-y-4">
            <div className="grid grid-cols-2 gap-4">
              <div className="space-y-2">
                <Label htmlFor="hostname">Hostname *</Label>
                <Input id="hostname" placeholder="airtime.local" value={form.hostname} onChange={update('hostname')} required />
              </div>
              <div className="space-y-2">
                <Label htmlFor="db_name">Database Name *</Label>
                <Input id="db_name" placeholder="airtime_local" value={form.db_name} onChange={update('db_name')} required />
              </div>
              <div className="space-y-2">
                <Label htmlFor="db_host">DB Host *</Label>
                <Input id="db_host" placeholder="localhost" value={form.db_host} onChange={update('db_host')} required />
              </div>
              <div className="space-y-2">
                <Label htmlFor="db_port">DB Port</Label>
                <Input id="db_port" type="number" value={form.db_port} onChange={update('db_port')} />
              </div>
              <div className="space-y-2">
                <Label htmlFor="db_user">DB User</Label>
                <Input id="db_user" value={form.db_user} onChange={update('db_user')} />
              </div>
              <div className="space-y-2">
                <Label htmlFor="db_password">DB Password *</Label>
                <Input id="db_password" type="password" value={form.db_password} onChange={update('db_password')} required />
              </div>
            </div>
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
