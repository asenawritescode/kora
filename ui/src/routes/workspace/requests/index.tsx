import { useState } from 'react'
import { useNavigate } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { ArrowRight, CheckCircle2, ClipboardCheck, RefreshCw } from 'lucide-react'
import type { InboxView, RequestView } from '@/generated/engine-v2'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Skeleton } from '@/components/ui/skeleton'
import { fetchNavigation, navigationQueryKey } from '@/lib/api/system'
import { fetchRequests } from '@/lib/api/requests'
import { getSitePrefix } from '@/lib/basepath'
import { firstCursorPage, nextCursorPage, previousCursorPage } from '@/lib/cursor-page'

const views: Array<{ id: InboxView; label: string }> = [
  { id: 'needs_action', label: 'Needs action' },
  { id: 'sent', label: 'Sent' },
  { id: 'waiting', label: 'Waiting' },
  { id: 'completed', label: 'Completed' },
]

export default function RequestsInboxPage() {
  const [view, setView] = useState<InboxView>('needs_action')
  const [page, setPage] = useState(firstCursorPage)
  const navigate = useNavigate()
  const { data: navigation } = useQuery({ queryKey: navigationQueryKey(), queryFn: fetchNavigation })
  const available = navigation?.supported_capabilities?.includes('requests.inbox.v2') ?? false
  const actor = navigation?.user?.name || 'current-user'
  const query = useQuery({
    queryKey: ['requests', getSitePrefix(), actor, view, page.cursor ?? 'first-page'],
    queryFn: () => fetchRequests({ view, kind: 'task', limit: 25, cursor: page.cursor }),
    enabled: available,
    staleTime: 10_000,
  })

  if (!available) {
    return (
      <div className="kora-page-frame kora-page-frame--wide">
        <header className="kora-page-header"><div><p className="kora-page-eyebrow">Workspace</p><h1 className="kora-page-title">Requests</h1><p className="kora-page-description">Request actions are not enabled for this tenant.</p></div></header>
        <Card className="border-dashed"><CardContent className="p-6 text-sm text-muted-foreground">This Engine does not advertise the request inbox capability.</CardContent></Card>
      </div>
    )
  }

  return (
    <div className="kora-page-frame kora-page-frame--wide">
      <header className="kora-page-header">
        <div><p className="kora-page-eyebrow">Workspace</p><h1 className="kora-page-title">Requests</h1><p className="kora-page-description">Direct tasks assigned to you, sent by you, and completed by your team.</p></div>
        <Badge variant="outline" className="gap-1"><ClipboardCheck className="h-3.5 w-3.5" /> User tasks</Badge>
      </header>

      <div className="flex flex-wrap gap-2" role="tablist" aria-label="Request inbox views">
        {views.map((item) => (
          <Button key={item.id} type="button" role="tab" aria-selected={view === item.id} variant={view === item.id ? 'default' : 'outline'} size="sm" onClick={() => { setView(item.id); setPage(firstCursorPage()) }}>{item.label}</Button>
        ))}
      </div>

      {query.isLoading ? <div className="space-y-3"><Skeleton className="h-24 w-full" /><Skeleton className="h-24 w-full" /></div> : null}
      {query.isError ? (
        <Card className="border-dashed"><CardContent className="p-6">
          <p className="font-medium">We couldn’t load this inbox.</p><p className="mt-1 text-sm text-muted-foreground">{query.error instanceof Error ? query.error.message : 'The request could not be loaded.'}</p>
          <Button variant="outline" size="sm" className="mt-4" onClick={() => { void query.refetch() }}><RefreshCw className="mr-2 h-4 w-4" />Try again</Button>
        </CardContent></Card>
      ) : null}
      {!query.isLoading && !query.isError && query.data?.items.length === 0 ? (
        <Card className="border-dashed"><CardContent className="flex items-center gap-3 p-6 text-sm text-muted-foreground"><CheckCircle2 className="h-5 w-5" />No requests in this view.</CardContent></Card>
      ) : null}
      {query.data?.items.map((request) => <RequestCard key={request.id} request={request} onOpen={() => { void navigate({ to: '/workspace/requests/$id' as never, params: { id: request.id } as never }) }} />)}
      <nav className="flex items-center justify-between" aria-label="Request inbox pages">
        <Button variant="outline" size="sm" disabled={query.isFetching || page.previous.length === 0} onClick={() => setPage((current) => previousCursorPage(current))}>Previous page</Button>
        <Button variant="outline" size="sm" disabled={query.isFetching || !query.data?.next_cursor} onClick={() => setPage((current) => nextCursorPage(current, query.data?.next_cursor))}>Next page</Button>
      </nav>
    </div>
  )
}

function RequestCard({ request, onOpen }: { request: RequestView; onOpen: () => void }) {
  const ownAssignment = request.allowed_responses.length > 0
  return (
    <Card>
      <CardHeader className="pb-3">
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div className="min-w-0"><CardTitle className="text-base">{request.title}</CardTitle><CardDescription className="mt-1">{request.message || 'No additional details.'}</CardDescription></div>
          <Badge variant={ownAssignment ? 'secondary' : 'outline'}>{ownAssignment ? 'Needs your action' : request.status}</Badge>
        </div>
      </CardHeader>
      <CardContent className="flex flex-wrap items-center justify-between gap-3 pt-0">
        <p className="text-xs text-muted-foreground">From {request.requester} · Updated {new Date(request.updated_at).toLocaleString()}</p>
        <Button size="sm" variant="outline" onClick={onOpen}>Open request <ArrowRight className="ml-2 h-4 w-4" /></Button>
      </CardContent>
    </Card>
  )
}
