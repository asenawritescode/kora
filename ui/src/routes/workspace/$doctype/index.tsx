import { useCallback, useMemo } from 'react'
import { useNavigate, useParams } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { fetchDoctypeSchema } from '@/lib/api/system'
import { Breadcrumbs } from '@/components/layout/Breadcrumbs'
import { Button } from '@/components/ui/button'
import { Skeleton } from '@/components/ui/skeleton'
import { Plus } from 'lucide-react'
import type { DocType } from '@/types/kora'
import { ManifestRenderer } from '@/manifest/runtime/ManifestRenderer'
import { createStandardPageManifest } from '@/manifest/runtime/standard-pages'
import { validatePageManifestContract } from '@/manifest/schema/page'
import { fetchPageManifestByRoute, executeViewAction } from '@/lib/api/page-manifests'
import { KoraApiError } from '@/lib/api/client'

export default function ListPage() {
  const { doctype } = useParams({ from: '/workspace/$doctype' })
  const navigate = useNavigate()
  const pageQuery = useQuery({
    queryKey: ['page-manifest-route', `/${doctype}`],
    queryFn: () => fetchPageManifestByRoute(`/${doctype}`),
    retry: false,
  })
  const isManifestNotFound = pageQuery.error instanceof KoraApiError && pageQuery.error.status === 404

  const schemaQuery = useQuery({
    queryKey: ['doctype', doctype],
    queryFn: () => fetchDoctypeSchema(doctype),
    staleTime: 5 * 60_000,
    enabled: isManifestNotFound,
  })

  const dt: DocType | undefined = schemaQuery.data?.doctype
  const manifest = useMemo(() => (dt ? createStandardPageManifest(dt, 'overview') : null), [dt])
  const issues = useMemo(() => (manifest ? validatePageManifestContract(manifest) : []), [manifest])
  const handleAction = useMemo(() => (actionId: string, context: Record<string, unknown>) => {
    if (actionId !== 'select') return
    const name = typeof context.name === 'string' ? context.name : ''
    if (!name) return
    navigate({ to: '/workspace/$doctype/$name', params: { doctype, name } })
  }, [doctype, navigate])
  const handleManifestAction = useCallback(async (actionId: string, context: Record<string, unknown>) => {
    const component = typeof context._component === 'string' ? context._component : ''
    if (!pageQuery.data || !component) return
    return executeViewAction(actionId, {
      view: pageQuery.data.metadata.name,
      component,
      context,
    })
  }, [pageQuery.data])

  if (pageQuery.isLoading) {
    return (
      <div className="p-8 space-y-4">
        <Skeleton className="h-8 w-48" />
        <Skeleton className="h-24 w-full" />
        <Skeleton className="h-64 w-full" />
      </div>
    )
  }

  if (pageQuery.data) {
    return (
      <div className="p-4 md:p-8 space-y-6">
        <div>
          <Breadcrumbs items={[{ label: pageQuery.data.metadata.package }, { label: pageQuery.data.metadata.name }]} className="mb-2" />
          <h1 className="text-2xl font-bold tracking-tight">{pageQuery.data.metadata.name}</h1>
        </div>
        <ManifestRenderer manifest={pageQuery.data} mode="runtime" onAction={handleManifestAction} />
      </div>
    )
  }

  if (!isManifestNotFound && pageQuery.isError) {
    return <div className="p-8 text-destructive" role="alert">Unable to resolve this workspace page: {pageQuery.error.message}</div>
  }

  if (schemaQuery.isLoading || !manifest) {
    return (
      <div className="p-8 space-y-4">
        <Skeleton className="h-8 w-48" />
        <Skeleton className="h-24 w-full" />
        <Skeleton className="h-64 w-full" />
      </div>
    )
  }

  if (schemaQuery.isError || !dt) {
    return (
      <div className="flex h-64 items-center justify-center">
        <p className="text-muted-foreground">DocType "{doctype}" not found.</p>
      </div>
    )
  }

  return (
    <div className="p-4 md:p-8 space-y-6">
      <div>
        <Breadcrumbs items={[...(dt.module ? [{ label: dt.module }] : []), { label: dt.name }]} className="mb-2" />

        <div className="flex flex-wrap items-start justify-between gap-3">
          <div>
            <h1 className="text-2xl font-bold tracking-tight">{dt.name}</h1>
          </div>
          <Button
            onClick={() => navigate({ to: '/workspace/$doctype/new', params: { doctype } })}
          >
            <Plus className="mr-2 h-4 w-4" />
            New {dt.name}
          </Button>
        </div>
      </div>

      {issues.length > 0 ? (
        <div className="rounded-xl border border-dashed bg-card p-6">
          <h2 className="text-lg font-semibold">Manifest needs attention</h2>
          <ul className="mt-4 space-y-2 text-sm">
            {issues.map((issue) => (
              <li key={`${issue.path}:${issue.message}`} className="rounded-lg bg-muted/40 p-2">
                <span className="font-mono text-xs text-destructive">{issue.path}</span>
                <span className="block text-muted-foreground">{issue.message}</span>
              </li>
            ))}
          </ul>
        </div>
      ) : (
        <ManifestRenderer manifest={manifest} mode="runtime" onAction={handleAction} />
      )}
    </div>
  )
}
