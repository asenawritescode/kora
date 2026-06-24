import { Link } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { fetchDoctypes, deleteDoctype } from '@/lib/api/system'
import { Button } from '@/components/ui/button'
import { Skeleton } from '@/components/ui/skeleton'
import { Badge } from '@/components/ui/badge'
import { Plus, Edit, Trash2, FileText } from 'lucide-react'
import { useState } from 'react'

export default function AdminDoctypesPage() {
  return (
    <div className="p-8">
      <div className="flex items-center justify-between mb-6">
        <div>
          <h1 className="text-3xl font-bold tracking-tight">DocTypes</h1>
          <p className="text-muted-foreground mt-1">Manage your data model</p>
        </div>
        <Link to="/workspace/admin/doctypes/new">
          <Button>
            <Plus className="h-4 w-4 mr-2" />
            New DocType
          </Button>
        </Link>
      </div>
      <DocTypeTable />
    </div>
  )
}

function DocTypeTable() {
  const { data, isLoading, error, refetch } = useQuery({
    queryKey: ['admin', 'doctypes'],
    queryFn: fetchDoctypes,
    staleTime: 30_000,
  })

  if (isLoading) {
    return (
      <div className="space-y-2">
        {[1, 2, 3, 4, 5].map((i) => (
          <Skeleton key={i} className="h-12 w-full" />
        ))}
      </div>
    )
  }

  if (error) {
    return (
      <div className="border border-dashed rounded-lg p-12 text-center">
        <p className="text-destructive font-medium">Failed to load doctypes</p>
        <p className="text-sm text-muted-foreground mt-1">{(error as Error).message}</p>
        <Button variant="outline" className="mt-4" onClick={() => refetch()}>
          Retry
        </Button>
      </div>
    )
  }

  if (!data || data.length === 0) {
    return (
      <div className="border-2 border-dashed rounded-lg p-12 text-center">
        <FileText className="h-12 w-12 mx-auto text-muted-foreground/40" />
        <h3 className="text-lg font-semibold mt-4">No DocTypes defined</h3>
        <p className="text-muted-foreground mt-1">
          Create your first doctype to start building your data model.
        </p>
        <Link to="/workspace/admin/doctypes/new">
          <Button className="mt-4">
            <Plus className="h-4 w-4 mr-2" />
            Create your first DocType
          </Button>
        </Link>
      </div>
    )
  }

  return (
    <div className="border rounded-lg">
      {/* Desktop table */}
      <table className="w-full hidden md:table">
        <thead>
          <tr className="border-b bg-muted/50">
            <th className="text-left px-4 py-3 text-sm font-medium">Name</th>
            <th className="text-left px-4 py-3 text-sm font-medium">Module</th>
            <th className="text-left px-4 py-3 text-sm font-medium">Fields</th>
            <th className="text-left px-4 py-3 text-sm font-medium">Status</th>
            <th className="text-left px-4 py-3 text-sm font-medium">Submittable</th>
            <th className="text-right px-4 py-3 text-sm font-medium">Actions</th>
          </tr>
        </thead>
        <tbody>
          {data.map((dt) => (
            <DoctypeRow key={dt.name} dt={dt} onDeleted={() => refetch()} />
          ))}
        </tbody>
      </table>
      {/* Mobile card layout */}
      <div className="md:hidden divide-y">
        {data.map((dt) => (
          <DoctypeCard key={dt.name} dt={dt} onDeleted={() => refetch()} />
        ))}
      </div>
    </div>
  )
}

function DoctypeRow({ dt, onDeleted }: { dt: any; onDeleted: () => void }) {
  const [deleting, setDeleting] = useState(false)

  const handleDelete = async () => {
    if (!confirm(`Delete "${dt.name}"?\n\nThis removes the configuration but does NOT drop the data table (tab${dt.name}).`)) return
    setDeleting(true)
    try {
      await deleteDoctype(dt.name)
      onDeleted()
    } catch (e) {
      alert((e as Error).message)
    } finally {
      setDeleting(false)
    }
  }

  return (
    <tr className="border-b hover:bg-muted/30">
      <td className="px-4 py-3 font-medium">{dt.name}</td>
      <td className="px-4 py-3 text-muted-foreground">{dt.module}</td>
      <td className="px-4 py-3">
        <span className="inline-flex items-center rounded-full bg-muted px-2 py-0.5 text-xs font-medium">
          {dt.fields?.length || 0} fields
        </span>
      </td>
      <td className="px-4 py-3">
        {dt.status === 'Active' ? (
          <Badge variant="default" className="bg-green-600 hover:bg-green-600">Active</Badge>
        ) : (
          <Badge variant="secondary" className="bg-amber-100 text-amber-800 hover:bg-amber-100">Draft</Badge>
        )}
      </td>
      <td className="px-4 py-3">
        {dt.is_submittable ? (
          <span className="inline-flex items-center rounded-full bg-green-100 text-green-800 dark:bg-green-900/30 dark:text-green-400 px-2 py-0.5 text-xs font-medium">
            Yes
          </span>
        ) : (
          <span className="text-muted-foreground text-sm">—</span>
        )}
      </td>
      <td className="px-4 py-3 text-right">
        <Link to="/workspace/admin/doctypes/$name" params={{ name: dt.name }}>
          <Button variant="ghost" size="sm">
            <Edit className="h-4 w-4" />
          </Button>
        </Link>
        <Button variant="ghost" size="sm" onClick={handleDelete} disabled={deleting}>
          <Trash2 className="h-4 w-4 text-destructive" />
        </Button>
      </td>
    </tr>
  )
}

function DoctypeCard({ dt, onDeleted }: { dt: any; onDeleted: () => void }) {
  const [deleting, setDeleting] = useState(false)

  const handleDelete = async () => {
    if (!confirm(`Delete "${dt.name}"?\n\nThis removes the configuration but does NOT drop the data table.`)) return
    setDeleting(true)
    try {
      await deleteDoctype(dt.name)
      onDeleted()
    } catch (e) {
      alert((e as Error).message)
    } finally {
      setDeleting(false)
    }
  }

  return (
    <div className="p-4 space-y-2">
      <div className="flex items-center justify-between">
        <span className="font-medium">{dt.name}</span>
        <div className="flex items-center gap-1">
          <Link to="/workspace/admin/doctypes/$name" params={{ name: dt.name }}>
            <Button variant="ghost" size="icon" className="h-8 w-8"><Edit className="h-4 w-4" /></Button>
          </Link>
          <Button variant="ghost" size="icon" className="h-8 w-8" onClick={handleDelete} disabled={deleting}>
            <Trash2 className="h-4 w-4 text-destructive" />
          </Button>
        </div>
      </div>
      <div className="flex items-center gap-2 text-sm text-muted-foreground">
        <span>{dt.module}</span>
        <span>·</span>
        <span className="inline-flex items-center rounded-full bg-muted px-2 py-0.5 text-xs font-medium">
          {dt.fields?.length || 0} fields
        </span>
      </div>
      <div className="flex items-center gap-2">
        {dt.status === 'Active' ? (
          <Badge variant="default" className="bg-green-600 hover:bg-green-600 text-xs">Active</Badge>
        ) : (
          <Badge variant="secondary" className="bg-amber-100 text-amber-800 hover:bg-amber-100 text-xs">Draft</Badge>
        )}
        {dt.is_submittable && (
          <span className="inline-flex items-center rounded-full bg-green-100 text-green-800 dark:bg-green-900/30 dark:text-green-400 px-2 py-0.5 text-xs font-medium">
            Submittable
          </span>
        )}
      </div>
    </div>
  )
}
