import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { fetchRoles, createRole, deleteRole } from '@/lib/api/system'
import { fetchDoctypes } from '@/lib/api/system'
import { fetchPermissions, savePermissions } from '@/lib/api/system'
import type { Role, Permission } from '@/lib/api/system'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Switch } from '@/components/ui/switch'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Skeleton } from '@/components/ui/skeleton'
import { Plus, Trash2, Save, ShieldCheck, UserPlus, Edit, ChevronDown, ChevronRight, X } from 'lucide-react'

const OPS = [
  { key: 'read', label: 'Read' },
  { key: 'write', label: 'Write' },
  { key: 'create', label: 'Create' },
  { key: 'delete', label: 'Delete' },
  { key: 'submit', label: 'Submit' },
  { key: 'cancel', label: 'Cancel' },
  { key: 'amend', label: 'Amend' },
  { key: 'export', label: 'Export' },
  { key: 'import', label: 'Import' },
  { key: 'report', label: 'Report' },
] as const

const EMPTY_PERMISSION: Permission = {
  doctype: '',
  role: '',
  read: false, write: false, create: false, delete: false,
  submit: false, cancel: false, amend: false,
  export: false, import: false, report: false,
  if_owner: false,
}

export default function AdminPermissionsPage() {
  const queryClient = useQueryClient()

  const { data: roles, isLoading: rolesLoading, refetch: refetchRoles } = useQuery({
    queryKey: ['admin', 'roles'],
    queryFn: fetchRoles,
  })
  const { data: doctypes } = useQuery({
    queryKey: ['admin', 'doctypes'],
    queryFn: fetchDoctypes,
  })
  const { data: permissions, isLoading, refetch } = useQuery({
    queryKey: ['admin', 'permissions'],
    queryFn: fetchPermissions,
  })

  const doctypeNames = doctypes?.map((d: any) => d.name) || []
  const roleNames = roles?.map((r: Role) => r.name) || []

  // State
  const [editing, setEditing] = useState<Permission | null>(null)
  const [addingRole, setAddingRole] = useState(false)
  const [newRole, setNewRole] = useState({ name: '', workspace_access: true, description: '' })
  const [saving, setSaving] = useState(false)
  const [expandedRole, setExpandedRole] = useState<string | null>(null)

  // Group permissions by role.
  const byRole = new Map<string, Permission[]>()
  permissions?.forEach((p) => {
    if (!byRole.has(p.role)) byRole.set(p.role, [])
    byRole.get(p.role)!.push(p)
  })

  const handleSavePermission = async () => {
    if (!editing || !permissions) return
    setSaving(true)
    try {
      const updated = permissions.filter(
        (p) => !(p.role === editing.role && p.doctype === editing.doctype)
      )
      updated.push(editing)
      await savePermissions(updated)
      setEditing(null)
      refetch()
    } catch (e) { alert((e as Error).message) }
    finally { setSaving(false) }
  }

  const handleDeletePermission = async (perm: Permission) => {
    if (!permissions || !confirm(`Delete: ${perm.role} on ${perm.doctype}?`)) return
    try {
      await savePermissions(permissions.filter((p) => !(p.role === perm.role && p.doctype === perm.doctype)))
      refetch()
    } catch (e) { alert((e as Error).message) }
  }

  const handleAddRole = async () => {
    if (!newRole.name) return
    try {
      await createRole(newRole)
      setAddingRole(false)
      setNewRole({ name: '', workspace_access: true, description: '' })
      refetchRoles()
    } catch (e) { alert((e as Error).message) }
  }

  const handleDeleteRole = async (name: string) => {
    if (!confirm(`Delete role "${name}"?`)) return
    try { await deleteRole(name); refetchRoles(); refetch() } catch (e) { alert((e as Error).message) }
  }

  return (
    <div className="p-4 sm:p-8 max-w-6xl">
      <div className="flex items-center gap-3 mb-6">
        <ShieldCheck className="h-6 w-6" />
        <div>
          <h1 className="text-2xl sm:text-3xl font-bold tracking-tight">Permissions</h1>
          <p className="text-muted-foreground mt-1 text-sm">Manage roles and access control</p>
        </div>
      </div>

      {/* Roles bar */}
      <div className="flex items-center gap-2 mb-4 flex-wrap">
        <span className="text-sm font-medium text-muted-foreground">Roles:</span>
        {roleNames.map((name) => (
          <span key={name} className="inline-flex items-center gap-1 rounded-full border px-3 py-1 text-sm">
            {name}
            <button className="text-muted-foreground hover:text-destructive ml-1" onClick={() => handleDeleteRole(name)}>
              <X className="h-3 w-3" />
            </button>
          </span>
        ))}
        <Button variant="outline" size="sm" onClick={() => setAddingRole(true)}>
          <UserPlus className="h-3 w-3 sm:mr-1" /> <span className="hidden sm:inline">Add Role</span>
        </Button>
      </div>

      {/* Add Role form */}
      {addingRole && (
        <div className="mb-4 p-4 border rounded-lg bg-muted/30 space-y-3 sm:space-y-0 sm:flex sm:items-end sm:gap-3">
          <div className="sm:flex-1">
            <label className="text-xs font-medium">Name *</label>
            <Input className="h-8 mt-1" value={newRole.name} onChange={(e) => setNewRole({ ...newRole, name: e.target.value })} placeholder="e.g. Sales Agent" />
          </div>
          <div className="sm:flex-1">
            <label className="text-xs font-medium">Description</label>
            <Input className="h-8 mt-1" value={newRole.description} onChange={(e) => setNewRole({ ...newRole, description: e.target.value })} />
          </div>
          <label className="flex items-center gap-2 text-sm pb-1">
            <Switch checked={newRole.workspace_access} onCheckedChange={(v) => setNewRole({ ...newRole, workspace_access: v })} />
            Workspace Access
          </label>
          <div className="flex gap-2">
            <Button size="sm" onClick={handleAddRole}>Save</Button>
            <Button size="sm" variant="ghost" onClick={() => setAddingRole(false)}>Cancel</Button>
          </div>
        </div>
      )}

      {/* Edit permission panel */}
      {editing && (
        <div className="mb-4 p-4 border rounded-lg bg-muted/30 space-y-3">
          <div className="flex flex-wrap items-center gap-3">
            <Select value={editing.role} onValueChange={(v) => setEditing({ ...editing, role: v || '' })}>
              <SelectTrigger className="w-[160px] h-8"><SelectValue placeholder="Role" /></SelectTrigger>
              <SelectContent>{roleNames.map((n) => <SelectItem key={n} value={n}>{n}</SelectItem>)}</SelectContent>
            </Select>
            <span className="text-muted-foreground text-sm">on</span>
            <Select value={editing.doctype} onValueChange={(v) => setEditing({ ...editing, doctype: v || '' })}>
              <SelectTrigger className="w-[160px] h-8"><SelectValue placeholder="DocType" /></SelectTrigger>
              <SelectContent>{doctypeNames.map((n) => <SelectItem key={n} value={n}>{n}</SelectItem>)}</SelectContent>
            </Select>
            <label className="flex items-center gap-1.5 text-sm">
              <Switch checked={editing.if_owner} onCheckedChange={(v) => setEditing({ ...editing, if_owner: v })} /> If Owner
            </label>
          </div>
          <div className="flex flex-wrap gap-2">
            {OPS.map((op) => (
              <label key={op.key} className="flex items-center gap-1 text-sm border rounded px-2 py-1 active:bg-muted">
                <Switch checked={(editing as any)[op.key]} onCheckedChange={(v) => setEditing({ ...editing, [op.key]: v })} />
                <span className="text-xs">{op.label}</span>
              </label>
            ))}
          </div>
          <div className="flex gap-2">
            <Button size="sm" onClick={handleSavePermission} disabled={saving}><Save className="h-4 w-4 mr-1" /> {saving ? 'Saving...' : 'Save'}</Button>
            <Button size="sm" variant="ghost" onClick={() => setEditing(null)}>Cancel</Button>
          </div>
        </div>
      )}

      {/* Desktop table */}
      <div className="hidden md:block border rounded-lg">
        {isLoading && <div className="p-4 space-y-2">{[1, 2, 3, 4].map((i) => <Skeleton key={i} className="h-10 w-full" />)}</div>}
        {!isLoading && permissions && permissions.length === 0 && (
          <div className="p-12 text-center">
            <ShieldCheck className="h-12 w-12 mx-auto text-muted-foreground/40" />
            <h3 className="text-lg font-semibold mt-4">No permissions defined</h3>
            <p className="text-muted-foreground mt-1">Add a permission to grant access to a role.</p>
          </div>
        )}
        {!isLoading && permissions && permissions.length > 0 && (
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b bg-muted/50">
                <th className="text-left px-4 py-2 font-medium">Role</th>
                <th className="text-left px-4 py-2 font-medium">DocType</th>
                {OPS.map((op) => <th key={op.key} className="text-center px-2 py-2 font-medium text-xs">{op.label}</th>)}
                <th className="text-center px-2 py-2 font-medium text-xs">Owner</th>
                <th className="text-right px-4 py-2 font-medium">Actions</th>
              </tr>
            </thead>
            <tbody>
              {permissions.map((p) => (
                <tr key={`${p.role}-${p.doctype}`} className="border-b hover:bg-muted/30">
                  <td className="px-4 py-2 font-medium">{p.role}</td>
                  <td className="px-4 py-2 text-muted-foreground">{p.doctype}</td>
                  {OPS.map((op) => <td key={op.key} className="text-center px-2 py-2">{(p as any)[op.key] ? '✓' : '—'}</td>)}
                  <td className="text-center px-2 py-2">{p.if_owner ? '✓' : '—'}</td>
                  <td className="px-4 py-2 text-right whitespace-nowrap">
                    <Button variant="ghost" size="sm" onClick={() => setEditing({ ...p })}>Edit</Button>
                    <Button variant="ghost" size="sm" onClick={() => handleDeletePermission(p)}><Trash2 className="h-4 w-4 text-destructive" /></Button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>

      {/* Mobile: role drill-down */}
      <div className="md:hidden space-y-2">
        {isLoading && <div className="space-y-2">{[1, 2, 3].map((i) => <Skeleton key={i} className="h-12 w-full" />)}</div>}

        {!isLoading && roleNames.length === 0 && (
          <div className="border-2 border-dashed rounded-lg p-8 text-center">
            <ShieldCheck className="h-10 w-10 mx-auto text-muted-foreground/40" />
            <h3 className="font-semibold mt-3">No roles defined</h3>
            <p className="text-sm text-muted-foreground mt-1">Add a role to get started.</p>
          </div>
        )}

        {!isLoading && roleNames.map((role) => {
          const rolePerms = byRole.get(role) || []
          const isExpanded = expandedRole === role
          return (
            <div key={role} className="border rounded-lg">
              <button
                className="w-full flex items-center justify-between p-4 text-left hover:bg-muted/30 transition-colors"
                onClick={() => setExpandedRole(isExpanded ? null : role)}
              >
                <div>
                  <div className="font-medium">{role}</div>
                  <div className="text-xs text-muted-foreground">{rolePerms.length} doctype{rolePerms.length !== 1 ? 's' : ''}</div>
                </div>
                <div className="flex items-center gap-2">
                  <Button variant="ghost" size="icon" className="h-8 w-8" onClick={(e) => { e.stopPropagation(); setEditing({ ...EMPTY_PERMISSION, role }) }}>
                    <Plus className="h-4 w-4" />
                  </Button>
                  {isExpanded ? <ChevronDown className="h-5 w-5 text-muted-foreground" /> : <ChevronRight className="h-5 w-5 text-muted-foreground" />}
                </div>
              </button>

              {isExpanded && (
                <div className="border-t divide-y">
                  {rolePerms.length === 0 && (
                    <div className="p-4 text-sm text-muted-foreground text-center">No permissions yet. Tap + to add.</div>
                  )}
                  {rolePerms.map((p) => {
                    const granted = OPS.filter((op) => (p as any)[op.key])
                    return (
                      <div key={`${p.role}-${p.doctype}`} className="p-4 space-y-2">
                        <div className="flex items-center justify-between">
                          <span className="font-medium text-sm">{p.doctype}</span>
                          <div className="flex gap-1">
                            <Button variant="ghost" size="icon" className="h-7 w-7" onClick={() => setEditing({ ...p })}>
                              <Edit className="h-3.5 w-3.5" />
                            </Button>
                            <Button variant="ghost" size="icon" className="h-7 w-7" onClick={() => handleDeletePermission(p)}>
                              <Trash2 className="h-3.5 w-3.5 text-destructive" />
                            </Button>
                          </div>
                        </div>
                        <div className="flex flex-wrap gap-1">
                          {granted.map((op) => (
                            <span key={op.key} className="inline-flex items-center rounded bg-primary/10 text-primary px-1.5 py-0.5 text-[11px] font-medium">{op.label}</span>
                          ))}
                          {granted.length === 0 && <span className="text-xs text-muted-foreground">No access</span>}
                          {p.if_owner && <span className="inline-flex items-center rounded bg-amber-100 text-amber-800 px-1.5 py-0.5 text-[11px] font-medium">Own</span>}
                        </div>
                      </div>
                    )
                  })}
                </div>
              )}
            </div>
          )
        })}
      </div>
    </div>
  )
}
