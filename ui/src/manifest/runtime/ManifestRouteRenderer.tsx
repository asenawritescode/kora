import { useSearch } from '@tanstack/react-router'
import { AlertTriangle } from 'lucide-react'
import { usePageManifest } from '../../lib/page-runtime'
import { ManifestRenderer } from './ManifestRenderer'
import { normalizeManifestRoute } from './policy'
import { validatePageManifestContract } from '../schema/page'
import { Skeleton } from '@/components/ui/skeleton'
import { executeViewAction } from '@/lib/api/page-manifests'

export function ManifestRouteRenderer({ route }: { route: string }) {
  const search = useSearch({ strict: false }) as { version?: string }
  const normalizedRoute = normalizeManifestRoute(route)
  const { data: manifest, isLoading, isError, error } = usePageManifest({ route: normalizedRoute, version: search.version })

  if (isLoading && !manifest) {
    return (
      <div className="space-y-4 p-8">
        <Skeleton className="h-8 w-48" />
        <div className="grid gap-4 lg:grid-cols-[1fr_300px]">
          <Skeleton className="h-96" />
          <Skeleton className="h-64" />
        </div>
      </div>
    )
  }

  if (isError || !manifest) {
    return (
      <div className="flex min-h-64 items-center justify-center p-8">
        <div className="max-w-lg rounded-xl border border-dashed bg-card p-6 text-center">
          <AlertTriangle className="mx-auto h-8 w-8 text-muted-foreground" />
          <h1 className="mt-3 text-lg font-semibold">Screen not found</h1>
          <p className="mt-2 text-sm text-muted-foreground">
            {error instanceof Error ? error.message : `The screen "${normalizedRoute}" could not be loaded.`}
          </p>
        </div>
      </div>
    )
  }

  const issues = validatePageManifestContract(manifest)
  if (issues.length > 0) {
    return (
      <div className="flex min-h-64 items-center justify-center p-8">
        <div className="max-w-xl rounded-xl border border-dashed bg-card p-6">
          <h1 className="text-lg font-semibold">Screen needs attention</h1>
          <p className="mt-2 text-sm text-muted-foreground">The page manifest failed validation and was not rendered.</p>
          <ul className="mt-4 space-y-2 text-sm">
            {issues.map((issue) => (
              <li key={`${issue.path}:${issue.message}`} className="rounded-lg bg-muted/40 p-2">
                <span className="font-mono text-xs text-destructive">{issue.path}</span>
                <span className="block text-muted-foreground">{issue.message}</span>
              </li>
            ))}
          </ul>
        </div>
      </div>
    )
  }

  const handleAction = async (actionId: string, context: Record<string, unknown>) => {
    if (actionId === 'filter' || actionId === 'search') return
    const component = typeof context._component === 'string' ? context._component : ''
    if (!component) return
    const cleanContext = { ...context }
    delete cleanContext._component
    return executeViewAction(actionId, {
      view: manifest.metadata.name,
      component,
      context: cleanContext,
    })
  }

  return (
    <div className="p-4 md:p-6">
      <div className="mb-6">
        <p className="mb-2 text-xs font-semibold uppercase tracking-[0.16em] text-muted-foreground">
          {manifest.metadata.package}
        </p>
        <h1 className="text-2xl font-bold tracking-tight">{manifest.metadata.name}</h1>
      </div>
      <ManifestRenderer manifest={manifest} mode="runtime" onAction={handleAction} />
    </div>
  )
}
