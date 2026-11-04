import { useState, useEffect } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useConsoleAuthStore } from '@/lib/console-auth-store'
import {
  listSites,
  updateSiteStorage,
  deleteSite,
} from '@/lib/api/console'
import type { ConsoleSite } from '@/types/api'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { PasswordInput } from '@/components/ui/password-input'
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
  Circle, Search, Server, Activity, Trash2, KeyRound, X,
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
      const resp = await fetch('/api/v1/ping')
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
          <p className="text-sm text-muted-foreground mt-1">Manage your Kigae sites</p>
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
              <p className="text-xs text-muted-foreground">Kigae Version</p>
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
                  <TableHead className="w-[60px]"></TableHead>
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
                    <TableCell>
                      <a
                        href={`/s/${s.name}/workspace`}
                        target="_blank"
                        rel="noopener noreferrer"
                        className="inline-flex items-center gap-1 text-xs text-muted-foreground hover:text-primary transition-colors"
                        title={`Open workspace login for ${s.name}`}
                        onClick={(e) => e.stopPropagation()}
                      >
                        <ExternalLink className="h-3.5 w-3.5" />
                        <span className="hidden sm:inline">Login</span>
                      </a>
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
  const [fileStorage, setFileStorage] = useState('local')
  const [storageBucket, setStorageBucket] = useState('')
  const [saving, setSaving] = useState(false)
  const [msg, setMsg] = useState<{ text: string; ok: boolean } | null>(null)

  // Delete state
  const [deleteConfirm, setDeleteConfirm] = useState('')
  const [deleting, setDeleting] = useState(false)
  const [deleteMsg, setDeleteMsg] = useState<{ text: string; ok: boolean } | null>(null)
  // Sync state when site changes.
  useEffect(() => {
    if (site) {
      setDomains(site.domains || [])
      setNewDomain('')
      setFileStorage(site.file_storage || 'local')
      setStorageBucket(site.storage_bucket || '')
      setMsg(null)
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
      const normalizedStorage = fileStorage.trim() || 'local'
      const normalizedBucket = normalizedStorage.toLowerCase() === 's3' ? storageBucket.trim() : ''
      await updateSiteStorage(site.name, {
        domains,
        file_storage: normalizedStorage,
        storage_bucket: normalizedBucket,
      })
      setMsg({ text: 'Site settings updated.', ok: true })
      onUpdated()
    } catch (err: any) {
      setMsg({ text: err.message, ok: false })
    }
    setSaving(false)
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
              Save Site Settings
            </Button>
          </div>

          {/* Storage */}
          <div className="space-y-3 border-t pt-6">
            <Label className="text-xs font-medium text-muted-foreground uppercase tracking-wider">Storage</Label>
            <div className="space-y-2">
              <div className="space-y-1.5">
                <Label className="text-xs text-muted-foreground">Backend</Label>
                <Select value={fileStorage} onValueChange={(value) => setFileStorage(value || 'local')}>
                  <SelectTrigger className="h-8 text-xs">
                    <SelectValue placeholder="Select backend" />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="local">local</SelectItem>
                    <SelectItem value="s3">s3</SelectItem>
                  </SelectContent>
                </Select>
              </div>
              {fileStorage === 's3' && (
                <div className="space-y-1.5">
                  <Label className="text-xs text-muted-foreground">Bucket</Label>
                  <Input
                    placeholder={`kora-${site?.name || 'site'}`}
                    value={storageBucket}
                    onChange={e => setStorageBucket(e.target.value)}
                    className="h-8 text-xs"
                  />
                  <p className="text-[11px] text-muted-foreground">
                    Leave empty to use the default bucket derived from the site name.
                  </p>
                </div>
              )}
            </div>
          </div>

          {/* Workspace Login */}
          <div className="space-y-3 border-t pt-6">
            <Label className="text-xs font-medium text-muted-foreground uppercase tracking-wider">Workspace Login</Label>
            <a
              href={`/s/${site?.name}/workspace`}
              target="_blank"
              rel="noopener noreferrer"
              className="inline-flex items-center gap-2 rounded-md border bg-muted/30 px-3 py-2 text-sm font-medium hover:bg-muted/50 transition-colors"
            >
              <ExternalLink className="h-4 w-4" />
              Open workspace for {site?.name}
            </a>
            <p className="text-xs text-muted-foreground">
              Path-based URL: <code className="text-xs bg-muted px-1 py-0.5 rounded">/s/{site?.name}/workspace</code>
            </p>
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
            <PasswordInput id="newPassword" value={pw} onChange={e => setPw(e.target.value)} required />
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
