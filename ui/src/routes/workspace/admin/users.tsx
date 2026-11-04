import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { fetchUsers, updateUser, fetchRoles } from '@/lib/api/system'
import type { User, UserUpdateRequest } from '@/lib/api/system'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import { Switch } from '@/components/ui/switch'
import { Skeleton } from '@/components/ui/skeleton'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Users, Pencil, Loader2, AlertCircle } from 'lucide-react'
import { cn } from '@/lib/utils'
import { HelpTooltip } from '@/components/ui/help-tooltip'
import { Label } from '@/components/ui/label'

export default function AdminUsersPage() {
  const queryClient = useQueryClient()
  const { data: users, isLoading, isError, error, refetch } = useQuery({
    queryKey: ['admin', 'users'],
    queryFn: fetchUsers,
  })
  const { data: roles } = useQuery({
    queryKey: ['admin', 'roles'],
    queryFn: fetchRoles,
  })

  const roleNames = roles?.map((role) => role.name) || []
  const [editingUser, setEditingUser] = useState<User | null>(null)
  const [saving, setSaving] = useState(false)
  const [formError, setFormError] = useState('')
  const [form, setForm] = useState({ roles: [] as string[], enabled: true })

  const openEdit = (user: User) => {
    setEditingUser(user)
    setForm({ roles: user.roles, enabled: user.enabled })
    setFormError('')
  }

  const toggleRole = (role: string) => {
    setForm((current) => ({
      ...current,
      roles: current.roles.includes(role)
        ? current.roles.filter((item) => item !== role)
        : [...current.roles, role],
    }))
  }

  const handleSave = async (event: React.FormEvent) => {
    event.preventDefault()
    if (!editingUser) return
    setSaving(true)
    setFormError('')
    try {
      const data: UserUpdateRequest = { roles: form.roles, enabled: form.enabled }
      await updateUser(editingUser.name, data)
      setEditingUser(null)
      await queryClient.invalidateQueries({ queryKey: ['admin', 'users'] })
    } catch (caught) {
      setFormError((caught as Error).message || 'Failed to update workspace access.')
    } finally {
      setSaving(false)
    }
  }

  return (
    <div className="space-y-6 p-8">
      <div>
        <h1 className="flex items-center gap-2 text-2xl font-semibold tracking-tight">
          Workspace users
          <HelpTooltip label="Workspace users help">
            Kora Cloud owns accounts and organization membership. Engine roles control permissions inside this workspace.
          </HelpTooltip>
        </h1>
        <p className="mt-1 text-sm text-muted-foreground">
          Account identity comes from Kora Cloud. Manage tenant roles and workspace access here.
        </p>
      </div>

      <Card>
        <CardHeader className="pb-3">
          <CardTitle className="flex items-center gap-2 text-base">
            <Users className="h-4 w-4" />
            Cloud-linked users
          </CardTitle>
          <CardDescription>{users?.length || 0} user(s)</CardDescription>
        </CardHeader>
        <CardContent>
          {isLoading ? (
            <div className="space-y-3">{[1, 2, 3].map((index) => <Skeleton key={index} className="h-12 w-full" />)}</div>
          ) : isError ? (
            <div className="flex flex-col items-center gap-3 rounded-lg border-2 border-dashed py-8 text-center">
              <AlertCircle className="h-8 w-8 text-destructive" />
              <p className="text-sm text-destructive">{(error as Error)?.message || 'Failed to load workspace users.'}</p>
              <Button variant="outline" size="sm" onClick={() => refetch()}>Retry</Button>
            </div>
          ) : !users || users.length === 0 ? (
            <div className="flex flex-col items-center gap-3 rounded-lg border-2 border-dashed py-10 text-center">
              <Users className="h-10 w-10 text-muted-foreground/50" />
              <div>
                <p className="text-sm font-medium">No Cloud-linked users yet</p>
                <p className="mt-1 text-xs text-muted-foreground">Workspace users appear here after Cloud provisions their access.</p>
              </div>
            </div>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Full name</TableHead>
                  <TableHead>Email</TableHead>
                  <TableHead>Engine roles</TableHead>
                  <TableHead>Workspace access</TableHead>
                  <TableHead>Created</TableHead>
                  <TableHead className="w-[72px]">Roles</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {users.map((user) => (
                  <TableRow key={user.name}>
                    <TableCell className="font-medium">{user.full_name}</TableCell>
                    <TableCell className="text-xs text-muted-foreground">{user.email}</TableCell>
                    <TableCell>
                      <div className="flex flex-wrap gap-1">
                        {user.roles.length === 0 ? <span className="text-xs text-muted-foreground">—</span> : user.roles.map((role) => (
                          <Badge key={role} variant="secondary" className="text-xs">{role}</Badge>
                        ))}
                      </div>
                    </TableCell>
                    <TableCell>
                      <Badge variant={user.enabled ? 'default' : 'destructive'} className="text-xs">
                        {user.enabled ? 'Enabled' : 'Disabled'}
                      </Badge>
                    </TableCell>
                    <TableCell className="text-xs text-muted-foreground">
                      {user.created ? new Date(user.created).toLocaleDateString() : '—'}
                    </TableCell>
                    <TableCell>
                      <Button variant="ghost" size="icon" className="h-8 w-8" onClick={() => openEdit(user)} title="Edit workspace roles">
                        <Pencil className="h-3.5 w-3.5" />
                      </Button>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>

      <Dialog open={editingUser !== null} onOpenChange={(open) => { if (!open) setEditingUser(null) }}>
        <DialogContent>
          <form onSubmit={(event) => void handleSave(event)}>
            <DialogHeader>
              <DialogTitle>Workspace access</DialogTitle>
              <DialogDescription>
                Kora Cloud manages this account. Set its Engine roles and access to this workspace.
              </DialogDescription>
            </DialogHeader>
            <div className="mt-4 space-y-4">
              <div>
                <Label>Account</Label>
                <p className="mt-1 text-sm font-medium">{editingUser?.full_name}</p>
                <p className="text-sm text-muted-foreground">{editingUser?.email}</p>
              </div>
              <div className="space-y-2">
                <Label>Engine roles</Label>
                {roleNames.length === 0 ? (
                  <p className="text-xs text-muted-foreground">No roles are defined for this workspace.</p>
                ) : (
                  <div className="flex flex-wrap gap-2">
                    {roleNames.map((role) => (
                      <button
                        key={role}
                        type="button"
                        onClick={() => toggleRole(role)}
                        className={cn(
                          'inline-flex items-center rounded-full border px-2.5 py-0.5 text-xs font-semibold transition-colors',
                          form.roles.includes(role)
                            ? 'border-primary bg-primary text-primary-foreground'
                            : 'border-input bg-background text-muted-foreground hover:border-primary/50',
                        )}
                      >
                        {role}
                      </button>
                    ))}
                  </div>
                )}
              </div>
              <div className="flex items-center justify-between">
                <Label htmlFor="workspace-user-enabled">Allow workspace access</Label>
                <Switch
                  id="workspace-user-enabled"
                  checked={form.enabled}
                  onCheckedChange={(enabled) => setForm((current) => ({ ...current, enabled }))}
                />
              </div>
              {formError && <p className="rounded-md bg-destructive/10 px-3 py-2 text-sm text-destructive">{formError}</p>}
            </div>
            <DialogFooter className="mt-6">
              <Button type="button" variant="outline" onClick={() => setEditingUser(null)}>Cancel</Button>
              <Button type="submit" disabled={saving}>
                {saving ? <Loader2 className="mr-1 h-4 w-4 animate-spin" /> : null}
                Save access
              </Button>
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>
    </div>
  )
}
