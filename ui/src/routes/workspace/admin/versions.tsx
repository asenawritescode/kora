import { useQuery } from '@tanstack/react-query'
import { api } from '@/lib/api/client'
import { activateVersion, discardVersion, rollbackVersion } from '@/lib/api/system'
import type { ConfigVersion } from '@/lib/api/system'
import { Button } from '@/components/ui/button'
import { Skeleton } from '@/components/ui/skeleton'
import { Badge } from '@/components/ui/badge'
import { History, Eye, Play, X, RotateCcw } from 'lucide-react'
import { useState } from 'react'

export default function AdminVersionsPage() {
  const { data, isLoading, error, refetch } = useQuery({
    queryKey: ['admin', 'versions'],
    queryFn: async () => {
      const result = await api.get<any[]>('/api/system/config/versions')
      return result as ConfigVersion[]
    },
    staleTime: 15_000,
  })

  const [acting, setActing] = useState<string | null>(null)
  const [viewingDiff, setViewingDiff] = useState<string | null>(null)
  const [diffData, setDiffData] = useState<any>(null)

  const handleActivate = async (id: string) => {
    if (!confirm('Activate this version? It will become the live configuration.')) return
    setActing(id)
    try {
      await activateVersion(id)
      refetch()
    } catch (e) { alert((e as Error).message) }
    finally { setActing(null) }
  }

  const handleDiscard = async (id: string) => {
    if (!confirm('Discard this draft? It will be marked as Superseded.')) return
    setActing(id)
    try {
      await discardVersion(id)
      refetch()
    } catch (e) { alert((e as Error).message) }
    finally { setActing(null) }
  }

  const handleRollback = async (id: string) => {
    if (!confirm('Rollback to this version? Current config will be replaced.')) return
    setActing(id)
    try {
      await rollbackVersion(id)
      refetch()
    } catch (e) { alert((e as Error).message) }
    finally { setActing(null) }
  }

  const viewDiff = async (id: string) => {
    if (viewingDiff === id) {
      setViewingDiff(null)
      setDiffData(null)
      return
    }
    setViewingDiff(id)
    try {
      // Find previous version to diff against.
      const versions = data || []
      const currentIdx = versions.findIndex((v) => v.id === id)
      const prev = versions[currentIdx + 1]
      if (prev) {
        const result = await api.get<any>(`/api/system/config/diff?from=${prev.id}&to=${id}`)
        setDiffData(result)
      } else {
        setDiffData({ changes: [{ message: 'No previous version to compare against.', type: 'info' }] })
      }
    } catch (e) {
      setDiffData({ changes: [{ message: (e as Error).message, type: 'error' }] })
    }
  }

  const statusBadge = (status: string) => {
    switch (status) {
      case 'Active': return <Badge variant="default" className="bg-green-600">Active</Badge>
      case 'Draft': return <Badge variant="secondary" className="bg-amber-100 text-amber-800">Draft</Badge>
      case 'Superseded': return <Badge variant="outline">Superseded</Badge>
      default: return <Badge variant="outline">{status}</Badge>
    }
  }

  return (
    <div className="p-8 max-w-5xl">
      <div className="flex items-center gap-3 mb-6">
        <History className="h-6 w-6" />
        <h1 className="text-3xl font-bold tracking-tight">Config Versions</h1>
      </div>

      {isLoading && (
        <div className="space-y-2">
          {[1, 2, 3, 4].map((i) => <Skeleton key={i} className="h-16 w-full" />)}
        </div>
      )}

      {error && (
        <div className="border border-destructive/50 rounded-lg p-6 text-center">
          <p className="text-destructive font-medium">Failed to load versions</p>
          <Button variant="outline" className="mt-2" onClick={() => refetch()}>Retry</Button>
        </div>
      )}

      {data && data.length === 0 && (
        <div className="border-2 border-dashed rounded-lg p-12 text-center">
          <History className="h-12 w-12 mx-auto text-muted-foreground/40" />
          <h3 className="text-lg font-semibold mt-4">No versions yet</h3>
          <p className="text-muted-foreground mt-1">Config versions are created when doctypes are saved.</p>
        </div>
      )}

      {data && data.length > 0 && (
        <GroupedVersionList
          data={data}
          statusBadge={statusBadge}
          viewDiff={viewDiff}
          viewingDiff={viewingDiff}
          diffData={diffData}
          acting={acting}
          handleActivate={handleActivate}
          handleDiscard={handleDiscard}
          handleRollback={handleRollback}
        />
      )}
    </div>
  )
}

function GroupedVersionList({ data, statusBadge, viewDiff, viewingDiff, diffData, acting, handleActivate, handleDiscard, handleRollback }: {
  data: ConfigVersion[]
  statusBadge: (s: string) => React.ReactNode
  viewDiff: (id: string) => void
  viewingDiff: string | null
  diffData: any
  acting: string | null
  handleActivate: (id: string) => Promise<void>
  handleDiscard: (id: string) => Promise<void>
  handleRollback: (id: string) => Promise<void>
}) {
  const drafts = data.filter(v => v.status === 'Draft')
  const active = data.filter(v => v.status === 'Active')
  const history = data.filter(v => v.status === 'Superseded')

  const handleActivateAll = async () => {
    if (!confirm(`Activate ALL ${drafts.length} Draft versions? This will activate the last Draft (v${drafts[0]?.version}), applying all accumulated changes.`)) return
    const lastDraft = drafts.reduce((a, b) => a.version > b.version ? a : b, drafts[0])
    if (lastDraft) await handleActivate(lastDraft.id)
  }

  const renderVersion = (v: ConfigVersion) => (
          <div key={v.id} className="border rounded-lg">
            <div className="flex items-center gap-4 px-4 py-3">
              <div className="font-mono text-sm font-bold">v{v.version}</div>
              {statusBadge(v.status)}
              <div className="flex-1">
                <div className="text-sm font-medium">{v.label}</div>
                <div className="text-xs text-muted-foreground">
                  by {v.created_by} &middot; {new Date(v.created_at).toLocaleString()}
                </div>
              </div>
              <div className="flex gap-1">
                <Button variant="ghost" size="sm" onClick={() => viewDiff(v.id)}>
                  <Eye className="h-4 w-4 mr-1" /> {viewingDiff === v.id ? 'Hide' : 'View'}
                </Button>
                {v.status === 'Draft' && (
                  <>
                    <Button variant="ghost" size="sm" onClick={() => handleActivate(v.id)} disabled={acting === v.id}>
                      <Play className="h-4 w-4 mr-1" /> Activate
                    </Button>
                    <Button variant="ghost" size="sm" onClick={() => handleDiscard(v.id)} disabled={acting === v.id}>
                      <X className="h-4 w-4 mr-1" /> Discard
                    </Button>
                  </>
                )}
                {v.status === 'Superseded' && (
                  <Button variant="ghost" size="sm" onClick={() => handleRollback(v.id)} disabled={acting === v.id}>
                    <RotateCcw className="h-4 w-4 mr-1" /> Rollback
                  </Button>
                )}
              </div>
            </div>

            {/* Diff view */}
            {viewingDiff === v.id && diffData && (
              <div className="border-t px-4 py-3 bg-muted/30">
                {diffData.changes?.map((c: any, i: number) => {
                  const colors: Record<string, string> = {
                    'doctype_added': 'text-green-700',
                    'field_added': 'text-green-700',
                    'constraint_added': 'text-green-700',
                    'doctype_removed': 'text-red-700',
                    'field_removed': 'text-red-700',
                    'constraint_removed': 'text-amber-700',
                    'field_type_changed': 'text-red-700',
                    'field_renamed': 'text-blue-700',
                    'field_required_changed': 'text-amber-700',
                  }
                  return (
                    <div key={i} className={`text-sm py-1 ${colors[c.type] || ''}`}>
                      {c.breaking && '🔴 '}
                      {c.message}
                    </div>
                  )
                })}
                {(!diffData.changes || diffData.changes.length === 0) && (
                  <div className="text-sm text-muted-foreground">No changes detected.</div>
                )}
              </div>
            )}
          </div>
        )

        return (
        <div className="space-y-6">
          {/* Pending Activation (Drafts) */}
          {drafts.length > 0 && (
            <div>
              <div className="flex items-center justify-between mb-2">
                <h2 className="text-sm font-semibold uppercase tracking-wider text-amber-600 flex items-center gap-2">
                  <span className="h-2 w-2 rounded-full bg-amber-500 inline-block" />
                  Pending Activation ({drafts.length})
                </h2>
                {drafts.length > 1 && (
                  <Button variant="outline" size="sm" onClick={handleActivateAll} className="text-xs">
                    <Play className="h-3 w-3 mr-1" /> Activate All
                  </Button>
                )}
              </div>
              <div className="space-y-2">
                {drafts.map(renderVersion)}
              </div>
            </div>
          )}

          {/* Active */}
          {active.length > 0 && (
            <div>
              <h2 className="text-sm font-semibold uppercase tracking-wider text-green-600 flex items-center gap-2 mb-2">
                <span className="h-2 w-2 rounded-full bg-green-500 inline-block" />
                Active ({active.length})
              </h2>
              <div className="space-y-2">
                {active.map(renderVersion)}
              </div>
            </div>
          )}

          {/* History (Superseded) */}
          {history.length > 0 && (
            <div>
              <h2 className="text-sm font-semibold uppercase tracking-wider text-muted-foreground flex items-center gap-2 mb-2">
                <span className="h-2 w-2 rounded-full bg-muted-foreground/30 inline-block" />
                History ({history.length})
              </h2>
              <div className="space-y-2">
                {history.map(renderVersion)}
              </div>
            </div>
          )}
        </div>
  )
}
