import { useQuery } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { fetchNavigation, navigationQueryKey } from '@/lib/api/system'
import { useAuthStore } from '@/lib/auth-store'
import { useUIStore } from '@/lib/ui-store'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Skeleton } from '@/components/ui/skeleton'
import { Button } from '@/components/ui/button'
import { LayoutGrid, ArrowRight, Star, Clock, CheckCircle2, Boxes, PlusCircle, Search, Filter, AlertCircle, RefreshCw } from 'lucide-react'
import { getFavorites, getRecentDoctypes, recordDoctypeVisit } from '@/lib/recent-doctypes'
import { listDocumentDrafts } from '@/lib/draft-storage'
import { useState, useEffect, useMemo } from 'react'
import type { DocTypeNavItem, ModuleGroup } from '@/types/api'
import type { LucideIcon } from 'lucide-react'
import { Input } from '@/components/ui/input'
import { Badge } from '@/components/ui/badge'
import { WORKSPACE_DASHBOARD_MANIFEST } from '@/lib/page-manifests'

export default function DashboardPage() {
  const { data, isLoading, isError, error, refetch } = useQuery({
    queryKey: navigationQueryKey(),
    queryFn: fetchNavigation,
    staleTime: 5 * 60_000,
  })

  const { user } = useAuthStore()
  const { setActiveModule } = useUIStore()
  const [favorites, setFavorites] = useState(getFavorites())
  const [recent, setRecent] = useState(getRecentDoctypes())
  const [drafts, setDrafts] = useState(listDocumentDrafts())
  const [query, setQuery] = useState('')

  useEffect(() => {
    const update = () => {
      setFavorites(getFavorites())
      setRecent(getRecentDoctypes())
      setDrafts(listDocumentDrafts())
    }
    window.addEventListener('storage', update)
    const interval = setInterval(update, 3000)
    return () => { window.removeEventListener('storage', update); clearInterval(interval) }
  }, [])

  const modules = data?.modules ?? []
  const firstModule = modules[0]
  const firstDoctype = firstModule?.doctypes[0]
  const normalizedQuery = query.trim().toLowerCase()
  const filteredModules = useMemo(() => {
    if (!normalizedQuery) return modules
    return modules
      .map((mod) => {
        const matchesModule = mod.label.toLowerCase().includes(normalizedQuery) || mod.module.toLowerCase().includes(normalizedQuery)
        const doctypes = mod.doctypes.filter((dt) =>
          dt.label.toLowerCase().includes(normalizedQuery) ||
          dt.name.toLowerCase().includes(normalizedQuery),
        )
        if (!matchesModule && doctypes.length === 0) return null
        return { ...mod, doctypes: matchesModule ? mod.doctypes : doctypes }
      })
      .filter(Boolean) as ModuleGroup[]
  }, [modules, normalizedQuery])

  const handleDoctypeClick = (doctype: DocTypeNavItem | { name: string; resource_name?: string; label: string; icon?: string }, module?: ModuleGroup) => {
    if (module) setActiveModule(module.module)
    recordDoctypeVisit(doctype)
  }

  const QuickList = ({ items, icon: Icon, title }: { items: { name: string; resource_name?: string; label: string; icon?: string }[], icon: LucideIcon, title: string }) => {
    if (items.length === 0) return null
    return (
      <div>
        <h2 className="mb-2 flex items-center gap-2 text-sm font-semibold uppercase tracking-wider text-muted-foreground">
          <Icon className="h-4 w-4" /> {title}
        </h2>
        <div className="flex gap-2 flex-wrap">
          {items.map(item => (
            <Link
              key={item.name}
              to="/workspace/$doctype"
              params={{ doctype: item.resource_name ?? item.name }}
              onClick={() => handleDoctypeClick(item)}
              className="rounded-full bg-muted px-3 py-1.5 text-sm transition-colors hover:bg-muted/80"
            >
              {item.label}
            </Link>
          ))}
        </div>
      </div>
    )
  }

  return (
    <div className="space-y-8 p-6 md:p-8">
      {/* Welcome */}
      <div>
        <p className="mb-2 text-xs font-semibold uppercase tracking-[0.16em] text-muted-foreground">
          {WORKSPACE_DASHBOARD_MANIFEST.label}
        </p>
        <h1 className="text-3xl font-bold tracking-tight">
          Welcome{user?.full_name ? `, ${user.full_name}` : ''}
        </h1>
        <p className="mt-1 text-muted-foreground">
          Start with one task, one record, or one workflow. Kigae keeps your work in one place so the team can move faster.
        </p>
      </div>

      <Card className="border-dashed bg-card/60" data-manifest-section="search">
        <CardContent className="flex flex-col gap-4 py-4 md:flex-row md:items-center md:justify-between">
          <div>
            <p className="text-xs font-medium uppercase tracking-wide text-muted-foreground">Find work fast</p>
            <p className="text-sm text-muted-foreground">Search modules, records, and forms in a few keystrokes.</p>
          </div>
          <div className="relative w-full md:max-w-md">
            <Search className="pointer-events-none absolute left-3 top-2.5 h-4 w-4 text-muted-foreground" />
            <Input
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              placeholder="Search a module, record, or form..."
              className="pl-9"
            />
          </div>
        </CardContent>
      </Card>

      <div className="grid gap-4 lg:grid-cols-[1.4fr_1fr]">
        <Card className="border-primary/20 bg-primary/5" data-manifest-section="hero">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-xl">
              <CheckCircle2 className="h-5 w-5 text-primary" />
              Start with the next useful task
            </CardTitle>
            <CardDescription>
              Open the record or workflow you need now. Kigae keeps the rest ready when you need it.
            </CardDescription>
          </CardHeader>
          <CardContent className="flex flex-wrap gap-3">
            {firstDoctype && firstModule ? (
              <Link
                to="/workspace/$doctype"
                params={{ doctype: firstDoctype.resource_name }}
                onClick={() => handleDoctypeClick(firstDoctype, firstModule)}
                className="inline-flex items-center gap-2 rounded-lg bg-primary px-4 py-2 text-sm font-medium text-primary-foreground transition-opacity hover:opacity-90"
              >
                <PlusCircle className="h-4 w-4" />
                Open {firstDoctype.label}
              </Link>
            ) : (
              <span className="text-sm text-muted-foreground">Add your first module to start tracking real work.</span>
            )}
            <a
              href="/api/v1/swagger-ui"
              target="_blank"
              rel="noopener noreferrer"
              className="inline-flex items-center gap-2 rounded-lg border px-4 py-2 text-sm font-medium transition-colors hover:bg-muted"
            >
              API reference
              <ArrowRight className="h-4 w-4" />
            </a>
          </CardContent>
        </Card>

        <Card data-manifest-section="resume">
          <CardHeader>
            <CardTitle>Resume work</CardTitle>
            <CardDescription>Favorites, recent records, and drafts stay one click away.</CardDescription>
          </CardHeader>
          <CardContent className="space-y-5">
            <QuickList items={favorites} icon={Star} title="Favorites" />
            <QuickList items={recent} icon={Clock} title="Recently viewed" />
            <DraftList items={drafts} />
            {favorites.length === 0 && recent.length === 0 && drafts.length === 0 && (
              <p className="text-sm text-muted-foreground">
                Star records from the sidebar or open a module to build your shortcuts.
              </p>
            )}
          </CardContent>
        </Card>
      </div>

      {/* Module grid */}
      {isError && !isLoading ? (
        <div className="rounded-lg border border-dashed border-destructive/50 p-12 text-center">
          <AlertCircle className="mx-auto h-12 w-12 text-destructive" />
          <h3 className="mt-4 text-lg font-medium">Failed to load navigation</h3>
          <p className="mt-2 text-sm text-muted-foreground">
            {error instanceof Error ? error.message : 'The server returned an error. Check your connection and try again.'}
          </p>
          <Button variant="outline" size="sm" className="mt-4" onClick={() => refetch()}>
            <RefreshCw className="mr-2 h-4 w-4" />
            Retry
          </Button>
        </div>
      ) : isLoading ? (
        <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
          {[1, 2, 3].map((i) => (
            <Skeleton key={i} className="h-32" />
          ))}
        </div>
      ) : (
        <div className="space-y-4">
          <div className="flex flex-wrap items-center gap-2">
            <Badge variant="secondary" className="gap-1">
              <Filter className="h-3.5 w-3.5" />
              {filteredModules.length} modules
            </Badge>
            {normalizedQuery && (
              <button
                type="button"
                onClick={() => setQuery('')}
                className="text-xs text-muted-foreground underline-offset-4 hover:underline"
              >
                Clear search
              </button>
            )}
          </div>

          <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3" data-manifest-section="modules">
            {filteredModules.map((mod) => (
            <Card key={mod.module} className="h-full">
              <CardHeader>
                <CardTitle className="flex items-center justify-between text-lg">
                  <span className="flex min-w-0 items-center gap-2">
                    <Boxes className="h-4 w-4 shrink-0 text-muted-foreground" />
                    <span className="truncate">{mod.label}</span>
                  </span>
                  <span className="rounded-full bg-muted px-2 py-0.5 text-xs font-normal text-muted-foreground">
                    {mod.doctypes.length}
                  </span>
                </CardTitle>
                <CardDescription>Open the records and workflows inside this area.</CardDescription>
              </CardHeader>
              <CardContent className="space-y-2">
                {mod.doctypes.slice(0, 5).map((dt) => (
                  <Link
                    key={dt.name}
                    to="/workspace/$doctype"
                    params={{ doctype: dt.name }}
                    onClick={() => handleDoctypeClick(dt, mod)}
                    className="group flex items-center justify-between rounded-lg px-3 py-2 text-sm transition-colors hover:bg-muted"
                  >
                    <span className="truncate">{dt.label}</span>
                    <ArrowRight className="h-4 w-4 shrink-0 text-muted-foreground opacity-0 transition-opacity group-hover:opacity-100" />
                  </Link>
                ))}
                {mod.doctypes.length > 5 && (
                  <p className="px-3 pt-1 text-xs text-muted-foreground">
                    {mod.doctypes.length - 5} more in this module
                  </p>
                )}
              </CardContent>
            </Card>
            ))}
          </div>

          {!filteredModules.length && normalizedQuery && (
            <div className="rounded-lg border border-dashed p-10 text-center">
              <LayoutGrid className="mx-auto h-10 w-10 text-muted-foreground" />
              <h3 className="mt-3 text-lg font-medium">No matches</h3>
              <p className="mt-1 text-sm text-muted-foreground">
                Try a different module name or document type.
              </p>
            </div>
          )}
        </div>
      )}

      {!isLoading && !isError && !data?.modules?.length && (
        <div className="rounded-lg border border-dashed p-12 text-center">
          <LayoutGrid className="mx-auto h-12 w-12 text-muted-foreground" />
          <h3 className="mt-4 text-lg font-medium">No modules configured yet</h3>
          <p className="mt-2 text-sm text-muted-foreground">
            Create your first DocType to start building your application.
          </p>
        </div>
      )}
    </div>
  )
}

function DraftList({ items }: { items: { doctype: string; name?: string; updatedAt: string }[] }) {
  if (items.length === 0) return null

  return (
    <div>
      <h2 className="mb-2 flex items-center gap-2 text-sm font-semibold uppercase tracking-wider text-muted-foreground">
        <Clock className="h-4 w-4" />
        Drafts
      </h2>
      <div className="space-y-2">
        {items.slice(0, 5).map((item) => (
          <Link
            key={`${item.doctype}:${item.name || 'new'}`}
            to={item.name ? '/workspace/$doctype/$name' : '/workspace/$doctype/new'}
            params={item.name ? { doctype: item.doctype, name: item.name } : { doctype: item.doctype }}
            className="flex items-center justify-between rounded-lg bg-muted px-3 py-2 text-sm transition-colors hover:bg-muted/80"
          >
            <span className="truncate">
              {item.doctype}
              {item.name ? ` / ${item.name}` : ' / new draft'}
            </span>
            <span className="ml-3 shrink-0 text-xs text-muted-foreground">
              {new Date(item.updatedAt).toLocaleDateString()}
            </span>
          </Link>
        ))}
      </div>
    </div>
  )
}
