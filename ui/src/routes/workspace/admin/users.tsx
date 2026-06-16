import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { fetchUsers, createUser, updateUser, deleteUser, resetUserPassword, fetchRoles } from '@/lib/api/system'
import type { User, UserCreateRequest, UserUpdateRequest } from '@/lib/api/system'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Badge } from '@/components/ui/badge'
import { Switch } from '@/components/ui/switch'
import { Skeleton } from '@/components/ui/skeleton'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Users, Plus, Pencil, Trash2, KeyRound, Loader2, AlertCircle } from 'lucide-react'
import { cn } from '@/lib/utils'

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

  const roleNames = roles?.map((r) => r.name) || []

  // Dialog state.
  const [dialogOpen, setDialogOpen] = useState(false)
  const [editingUser, setEditingUser] = useState<User | null>(null)
  const [saving, setSaving] = useState(false)
  const [formError, setFormError] = useState('')

  // Reset password state.
  const [resetOpen, setResetOpen] = useState(false)
  const [resetTarget, setResetTarget] = useState<User | null>(null)
  const [resetPassword, setResetPassword] = useState('')
  const [resetConfirm, setResetConfirm] = useState('')
  const [resetError, setResetError] = useState('')
  const [resetting, setResetting] = useState(false)

  // Delete state.
  const [deleteOpen, setDeleteOpen] = useState(false)
  const [deleteTarget, setDeleteTarget] = useState<User | null>(null)
  const [deleting, setDeleting] = useState(false)

  // Form state.
  const [form, setForm] = useState({
    email: '',
    full_name: '',
    password: '',
    roles: [] as string[],
    enabled: true,
  })

  const openCreate = () => {
    setEditingUser(null)
    setForm({ email: '', full_name: '', password: '', roles: [], enabled: true })
    setFormError('')
    setDialogOpen(true)
  }

  const openEdit = (u: User) => {
    setEditingUser(u)
    setForm({
      email: u.email,
      full_name: u.full_name,
      password: '',
      roles: u.roles,
      enabled: u.enabled,
    })
    setFormError('')
    setDialogOpen(true)
  }

  const toggleRole = (role: string) => {
    setForm((f) => ({
      ...f,
      roles: f.roles.includes(role) ? f.roles.filter((r) => r !== role) : [...f.roles, role],
    }))
  }

  const handleSave = async (e: React.FormEvent) => {
    e.preventDefault()
    setFormError('')

    if (!form.email || !form.full_name) {
      setFormError('Email and full name are required.')
      return
    }

    if (!editingUser && !form.password) {
      setFormError('Password is required for new users.')
      return
    }

    if (form.password && form.password.length < 8) {
      setFormError('Password must be at least 8 characters.')
      return
    }

    setSaving(true)
    try {
      if (editingUser) {
        const data: UserUpdateRequest = {
          full_name: form.full_name,
          roles: form.roles,
          enabled: form.enabled,
        }
        if (form.password) data.password = form.password
        await updateUser(editingUser.name, data)
      } else {
        const data: UserCreateRequest = {
          email: form.email,
          full_name: form.full_name,
          password: form.password,
          roles: form.roles,
        }
        await createUser(data)
      }
      setDialogOpen(false)
      queryClient.invalidateQueries({ queryKey: ['admin', 'users'] })
    } catch (err: any) {
      setFormError(err.message || 'Failed to save user')
    }
    setSaving(false)
  }

  const handleResetPassword = async (e: React.FormEvent) => {
    e.preventDefault()
    setResetError('')
    if (resetPassword.length < 8) {
      setResetError('Password must be at least 8 characters.')
      return
    }
    if (resetPassword !== resetConfirm) {
      setResetError('Passwords do not match.')
      return
    }
    if (!resetTarget) return
    setResetting(true)
    try {
      await resetUserPassword(resetTarget.name, { password: resetPassword })
      setResetOpen(false)
    } catch (err: any) {
      setResetError(err.message || 'Failed to reset password')
    }
    setResetting(false)
  }

  const handleDelete = async () => {
    if (!deleteTarget) return
    setDeleting(true)
    try {
      await deleteUser(deleteTarget.name)
      setDeleteOpen(false)
      queryClient.invalidateQueries({ queryKey: ['admin', 'users'] })
    } catch (err: any) {
      setDeleting(false)
    }
  }

  return (
    <div className="p-8 space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">Users</h1>
          <p className="text-sm text-muted-foreground mt-1">Manage users for this site</p>
        </div>
        <Button onClick={openCreate} size="sm">
          <Plus className="h-4 w-4 mr-1" />
          Add User
        </Button>
      </div>

      <Card>
        <CardHeader className="pb-3">
          <CardTitle className="flex items-center gap-2 text-base">
            <Users className="h-4 w-4" />
            All Users
          </CardTitle>
          <CardDescription>{users?.length || 0} user(s)</CardDescription>
        </CardHeader>
        <CardContent>
          {isLoading ? (
            <div className="space-y-3">
              {[1, 2, 3].map((i) => (
                <Skeleton key={i} className="h-12 w-full" />
              ))}
            </div>
          ) : isError ? (
            <div className="flex flex-col items-center gap-3 py-8 text-center border-2 border-dashed rounded-lg">
              <AlertCircle className="h-8 w-8 text-destructive" />
              <p className="text-sm text-destructive">{(error as Error)?.message || 'Failed to load users'}</p>
              <Button variant="outline" size="sm" onClick={() => refetch()}>Retry</Button>
            </div>
          ) : !users || users.length === 0 ? (
            <div className="flex flex-col items-center gap-3 py-10 text-center border-2 border-dashed rounded-lg">
              <Users className="h-10 w-10 text-muted-foreground/50" />
              <div>
                <p className="text-sm font-medium">No users yet</p>
                <p className="text-xs text-muted-foreground mt-1">Create the first user for this site.</p>
              </div>
              <Button variant="outline" size="sm" onClick={openCreate}>
                <Plus className="h-4 w-4 mr-1" /> Add User
              </Button>
            </div>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Full Name</TableHead>
                  <TableHead>Email</TableHead>
                  <TableHead>Roles</TableHead>
                  <TableHead>Status</TableHead>
                  <TableHead>Created</TableHead>
                  <TableHead className="w-[120px]">Actions</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {users.map((u) => (
                  <TableRow key={u.name}>
                    <TableCell className="font-medium">{u.full_name}</TableCell>
                    <TableCell className="text-xs text-muted-foreground">{u.email}</TableCell>
                    <TableCell>
                      <div className="flex flex-wrap gap-1">
                        {u.roles.length === 0 ? (
                          <span className="text-xs text-muted-foreground">—</span>
                        ) : (
                          u.roles.map((r) => (
                            <Badge key={r} variant="secondary" className="text-xs">{r}</Badge>
                          ))
                        )}
                      </div>
                    </TableCell>
                    <TableCell>
                      <Badge variant={u.enabled ? 'default' : 'destructive'} className="text-xs">
                        {u.enabled ? 'Enabled' : 'Disabled'}
                      </Badge>
                    </TableCell>
                    <TableCell className="text-xs text-muted-foreground">
                      {u.created ? new Date(u.created).toLocaleDateString() : '—'}
                    </TableCell>
                    <TableCell>
                      <div className="flex items-center gap-1">
                        <Button variant="ghost" size="icon" className="h-8 w-8" onClick={() => openEdit(u)} title="Edit">
                          <Pencil className="h-3.5 w-3.5" />
                        </Button>
                        <Button variant="ghost" size="icon" className="h-8 w-8" onClick={() => { setResetTarget(u); setResetPassword(''); setResetConfirm(''); setResetError(''); setResetOpen(true) }} title="Reset Password">
                          <KeyRound className="h-3.5 w-3.5" />
                        </Button>
                        <Button variant="ghost" size="icon" className="h-8 w-8 text-destructive hover:text-destructive" onClick={() => { setDeleteTarget(u); setDeleteOpen(true) }} title="Delete">
                          <Trash2 className="h-3.5 w-3.5" />
                        </Button>
                      </div>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>

      {/* Create / Edit Dialog */}
      <Dialog open={dialogOpen} onOpenChange={setDialogOpen}>
        <DialogContent>
          <form onSubmit={handleSave}>
            <DialogHeader>
              <DialogTitle>{editingUser ? 'Edit User' : 'Add User'}</DialogTitle>
              <DialogDescription>
                {editingUser ? 'Update user profile and permissions.' : 'Create a new user account for this site.'}
              </DialogDescription>
            </DialogHeader>

            <div className="space-y-4 mt-4">
              <div className="space-y-2">
                <Label htmlFor="email">Email *</Label>
                <Input
                  id="email"
                  type="email"
                  value={form.email}
                  onChange={(e) => setForm((f) => ({ ...f, email: e.target.value }))}
                  disabled={!!editingUser}
                  required
                />
              </div>

              <div className="space-y-2">
                <Label htmlFor="full_name">Full Name *</Label>
                <Input
                  id="full_name"
                  value={form.full_name}
                  onChange={(e) => setForm((f) => ({ ...f, full_name: e.target.value }))}
                  required
                />
              </div>

              <div className="space-y-2">
                <Label htmlFor="password">{editingUser ? 'New Password (leave blank to keep current)' : 'Password *'}</Label>
                <Input
                  id="password"
                  type="password"
                  value={form.password}
                  onChange={(e) => setForm((f) => ({ ...f, password: e.target.value }))}
                  placeholder={editingUser ? 'Leave blank to keep current' : 'Min 8 characters'}
                  required={!editingUser}
                />
              </div>

              {/* Roles multi-select */}
              <div className="space-y-2">
                <Label>Roles</Label>
                {roleNames.length === 0 ? (
                  <p className="text-xs text-muted-foreground">No roles defined. User will have no permissions.</p>
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
                            ? 'bg-primary border-primary text-primary-foreground'
                            : 'border-input bg-background text-muted-foreground hover:border-primary/50',
                        )}
                      >
                        {role}
                      </button>
                    ))}
                  </div>
                )}
              </div>

              {/* Enabled toggle */}
              <div className="flex items-center justify-between">
                <Label htmlFor="enabled">Account Enabled</Label>
                <Switch
                  id="enabled"
                  checked={form.enabled}
                  onCheckedChange={(v) => setForm((f) => ({ ...f, enabled: v }))}
                />
              </div>

              {formError && (
                <p className="text-sm text-destructive bg-destructive/10 rounded-md px-3 py-2">{formError}</p>
              )}
            </div>

            <DialogFooter className="mt-6">
              <Button type="button" variant="outline" onClick={() => setDialogOpen(false)}>Cancel</Button>
              <Button type="submit" disabled={saving}>
                {saving ? <Loader2 className="h-4 w-4 mr-1 animate-spin" /> : null}
                {editingUser ? 'Save Changes' : 'Create User'}
              </Button>
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>

      {/* Reset Password Dialog */}
      <Dialog open={resetOpen} onOpenChange={setResetOpen}>
        <DialogContent>
          <form onSubmit={handleResetPassword}>
            <DialogHeader>
              <DialogTitle>Reset Password</DialogTitle>
              <DialogDescription>
                Set a new password for <strong>{resetTarget?.full_name || resetTarget?.email}</strong>.
                All existing sessions will be invalidated.
              </DialogDescription>
            </DialogHeader>

            <div className="space-y-4 mt-4">
              <div className="space-y-2">
                <Label htmlFor="reset_password">New Password</Label>
                <Input
                  id="reset_password"
                  type="password"
                  value={resetPassword}
                  onChange={(e) => setResetPassword(e.target.value)}
                  placeholder="Min 8 characters"
                  required
                />
              </div>
              <div className="space-y-2">
                <Label htmlFor="reset_confirm">Confirm Password</Label>
                <Input
                  id="reset_confirm"
                  type="password"
                  value={resetConfirm}
                  onChange={(e) => setResetConfirm(e.target.value)}
                  placeholder="Re-enter password"
                  required
                />
              </div>
              {resetError && (
                <p className="text-sm text-destructive bg-destructive/10 rounded-md px-3 py-2">{resetError}</p>
              )}
            </div>

            <DialogFooter className="mt-6">
              <Button type="button" variant="outline" onClick={() => setResetOpen(false)}>Cancel</Button>
              <Button type="submit" disabled={resetting}>
                {resetting ? <Loader2 className="h-4 w-4 mr-1 animate-spin" /> : null}
                Reset Password
              </Button>
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>

      {/* Delete Confirmation */}
      <Dialog open={deleteOpen} onOpenChange={setDeleteOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete User</DialogTitle>
            <DialogDescription>
              Are you sure you want to delete <strong>{deleteTarget?.full_name || deleteTarget?.email}</strong>?
              This action cannot be undone. All sessions for this user will be terminated.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter className="mt-4">
            <Button variant="outline" onClick={() => setDeleteOpen(false)}>Cancel</Button>
            <Button variant="destructive" onClick={handleDelete} disabled={deleting}>
              {deleting ? <Loader2 className="h-4 w-4 mr-1 animate-spin" /> : null}
              Delete User
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}
