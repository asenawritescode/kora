import { useEffect, useLayoutEffect, useRef, useState } from 'react'
import { useNavigate, useParams } from '@tanstack/react-router'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { AlertTriangle, ArrowLeft, CheckCircle2, Clock3, Loader2, RefreshCw } from 'lucide-react'
import type { ResponseKind } from '@/generated/engine-v2'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Textarea } from '@/components/ui/textarea'
import { KoraApiError } from '@/lib/api/client'
import { fetchNavigation, navigationQueryKey } from '@/lib/api/system'
import { fetchRequest, fetchRequestHistory, idempotencyKey, markRequestRead, respondToRequest } from '@/lib/api/requests'
import { getSitePrefix } from '@/lib/basepath'
import { requestDetailScope } from '@/lib/request-detail-scope'
import { firstCursorPage, nextCursorPage, previousCursorPage, type CursorPageState } from '@/lib/cursor-page'

const responseLabels: Record<ResponseKind, string> = {
  approve: 'Approve',
  reject: 'Reject',
  return_for_changes: 'Return for changes',
  provide_information: 'Provide information',
  acknowledge: 'Acknowledge',
  complete: 'Complete task',
}

export default function RequestDetailPage() {
  const { id } = useParams({ strict: false }) as { id: string }
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  const { data: navigation } = useQuery({ queryKey: navigationQueryKey(), queryFn: fetchNavigation })
  const available = navigation?.supported_capabilities?.includes('requests.inbox.v2') ?? false
  const actor = navigation?.user?.name || 'current-user'
  const requestScope = requestDetailScope(getSitePrefix(), actor, id)
  const [commentState, setCommentState] = useState({ scope: requestScope, value: '' })
  const comment = commentState.scope === requestScope ? commentState.value : ''
  const setComment = (value: string) => setCommentState({ scope: requestScope, value })
  const [respondingState, setRespondingState] = useState({ scope: requestScope, value: false })
  const responding = respondingState.scope === requestScope && respondingState.value
  const setResponding = (value: boolean) => setRespondingState({ scope: requestScope, value })
  const [recoveryState, setRecoveryState] = useState({ scope: requestScope, value: '' })
  const recovery = recoveryState.scope === requestScope ? recoveryState.value : ''
  const setRecovery = (value: string) => setRecoveryState({ scope: requestScope, value })
  const [historyPageState, setHistoryPageState] = useState(() => ({ scope: requestScope, page: firstCursorPage() }))
  const historyPage = historyPageState.scope === requestScope ? historyPageState.page : firstCursorPage()
  const updateHistoryPage = (update: (current: CursorPageState) => CursorPageState) => {
    setHistoryPageState((current) => ({
      scope: requestScope,
      page: update(current.scope === requestScope ? current.page : firstCursorPage()),
    }))
  }
  const [readErrorState, setReadErrorState] = useState({ scope: requestScope, value: '' })
  const readError = readErrorState.scope === requestScope ? readErrorState.value : ''
  const pendingKey = useRef<{ scope: string; key: string } | null>(null)
  const readAttempted = useRef<string | null>(null)
  const activeScope = useRef(requestScope)
  useLayoutEffect(() => { activeScope.current = requestScope }, [requestScope])
  const keyPrefix = ['requests', getSitePrefix(), actor]
  const request = useQuery({
    queryKey: [...keyPrefix, 'detail', id],
    queryFn: () => fetchRequest(id),
    enabled: available && Boolean(id),
  })
  const history = useQuery({
    queryKey: [...keyPrefix, 'history', id, historyPage.cursor ?? 'first-page'],
    queryFn: () => fetchRequestHistory(id, { limit: 50, cursor: historyPage.cursor }),
    enabled: available && Boolean(id),
  })

  useEffect(() => {
    if (!available || !id || readAttempted.current === requestScope) return
    readAttempted.current = requestScope
    void markRequestRead(id).then(() => setReadErrorState({ scope: requestScope, value: '' })).catch(() => setReadErrorState({ scope: requestScope, value: 'We could not save your read receipt. Your request remains available.' }))
  }, [available, id, requestScope])

  const submitResponse = async (response: ResponseKind) => {
    if (!request.data) return
    const assignment = request.data.assignments.find((item) => item.status === 'pending' && item.allowed_responses.includes(response))
    if (!assignment) {
      setRecovery('Your assignment is no longer active. Refresh this request before taking another action.')
      await request.refetch()
      return
    }
    if (pendingKey.current?.scope !== requestScope) pendingKey.current = { scope: requestScope, key: idempotencyKey() }
    setResponding(true)
    setRecovery('')
    try {
      await respondToRequest(id, {
        meta: { idempotency_key: pendingKey.current.key, expected_revision: request.data.revision },
        assignment_id: assignment.id,
        response,
        comment,
      })
      if (activeScope.current !== requestScope) return
      pendingKey.current = null
      setComment('')
      await queryClient.invalidateQueries({ queryKey: ['requests'] })
      await request.refetch()
      await history.refetch()
    } catch (error) {
      if (activeScope.current !== requestScope) return
      if (error instanceof KoraApiError && (error.status === 409 || error.code === 'STALE_SUBJECT')) {
        pendingKey.current = null
        setRecovery('This request changed before your response was recorded. Refresh the request and review its current state before responding again.')
        await request.refetch()
        await history.refetch()
      } else if (error instanceof KoraApiError && (error.code === 'OUTCOME_UNKNOWN' || error.status >= 500)) {
        setRecovery('The server could not confirm the outcome. Your retry key is preserved; retrying will not create a second decision.')
      } else {
        pendingKey.current = null
        setRecovery(error instanceof Error ? error.message : 'Your response could not be saved.')
      }
    } finally {
      if (activeScope.current === requestScope) setResponding(false)
    }
  }

  if (!available) {
    return <div className="kora-page-frame kora-page-frame--wide"><Card className="border-dashed"><CardContent className="p-6 text-sm text-muted-foreground">The request inbox is not enabled for this tenant.</CardContent></Card></div>
  }

  return (
    <div className="kora-page-frame kora-page-frame--wide space-y-6">
      <Button variant="ghost" size="sm" className="-ml-2" onClick={() => { void navigate({ to: '/workspace/requests' }) }}><ArrowLeft className="mr-2 h-4 w-4" />Back to requests</Button>
      {request.isLoading ? <Card><CardContent className="p-6"><Loader2 className="h-5 w-5 animate-spin" aria-label="Loading request" /></CardContent></Card> : null}
      {request.isError ? <Card className="border-dashed"><CardContent className="p-6"><p className="font-medium">We couldn’t load this request.</p><p className="mt-1 text-sm text-muted-foreground">{request.error instanceof Error ? request.error.message : 'Try again.'}</p><Button variant="outline" size="sm" className="mt-4" onClick={() => { void request.refetch() }}><RefreshCw className="mr-2 h-4 w-4" />Retry</Button></CardContent></Card> : null}
      {request.data ? (
        <>
          <header className="kora-page-header"><div><p className="kora-page-eyebrow">Direct task · {request.data.id}</p><h1 className="kora-page-title">{request.data.title}</h1><p className="kora-page-description">{request.data.message}</p></div><Badge variant={request.data.status === 'pending' ? 'secondary' : 'outline'}>{request.data.status}</Badge></header>
          {request.data.status === 'stale' && <Card className="border-amber-500/40 bg-amber-500/5"><CardContent className="flex items-start gap-3 p-4 text-sm"><AlertTriangle className="mt-0.5 h-4 w-4 text-amber-600" /><p>This request is stale and can no longer be completed. Ask the requester to submit a fresh task.</p></CardContent></Card>}
          {recovery && <Card className="border-amber-500/40 bg-amber-500/5"><CardContent className="flex items-start gap-3 p-4 text-sm"><AlertTriangle className="mt-0.5 h-4 w-4 text-amber-600" /><p role="alert">{recovery}</p></CardContent></Card>}
          {readError && <p role="status" className="text-sm text-muted-foreground">{readError}</p>}
          <div className="grid gap-5 lg:grid-cols-[minmax(0,1.4fr)_minmax(18rem,0.8fr)]">
            <Card><CardHeader><CardTitle className="text-base">Assignment</CardTitle><CardDescription>Responses are checked against the current assignment and request revision.</CardDescription></CardHeader><CardContent className="space-y-4">
              <p className="text-sm text-muted-foreground">Requested by <span className="font-medium text-foreground">{request.data.requester}</span> · Revision {request.data.revision}</p>
              {request.data.due_at && <p className="flex items-center gap-2 text-sm text-muted-foreground"><Clock3 className="h-4 w-4" />Due {new Date(request.data.due_at).toLocaleString()}</p>}
              <div className="space-y-2"><h2 className="text-sm font-semibold">People assigned</h2>{request.data.assignments.map((assignment) => <div key={assignment.id} className="flex items-center justify-between rounded-md border p-3 text-sm"><span>{assignment.assignee.id}</span><Badge variant="outline">{assignment.status}</Badge></div>)}</div>
              {request.data.status === 'pending' && request.data.allowed_responses.length > 0 && <div className="space-y-3 border-t pt-4"><label htmlFor="request-comment" className="text-sm font-medium">Comment <span className="font-normal text-muted-foreground">(optional)</span></label><Textarea id="request-comment" value={comment} onChange={(event) => setComment(event.target.value)} maxLength={8192} rows={3} placeholder="Add context for the requester" /><div className="flex flex-wrap gap-2">{request.data.allowed_responses.map((response) => <Button key={response} disabled={responding} onClick={() => { void submitResponse(response) }}>{responding && <Loader2 className="mr-2 h-4 w-4 animate-spin" />}{responseLabels[response]}</Button>)}</div></div>}
              {request.data.status === 'resolved' && <p className="flex items-center gap-2 text-sm text-muted-foreground"><CheckCircle2 className="h-4 w-4" />This request has been completed.</p>}
            </CardContent></Card>
            <Card><CardHeader><CardTitle className="text-base">Assignment history</CardTitle><CardDescription>Immutable request history and decisions.</CardDescription></CardHeader><CardContent className="space-y-3">
              {history.isLoading && <Loader2 className="h-5 w-5 animate-spin" aria-label="Loading history" />}
              {history.isError && <div className="text-sm text-muted-foreground"><p>History could not be loaded.</p><Button variant="link" size="sm" onClick={() => { void history.refetch() }}>Retry</Button></div>}
              {history.data?.items.map((entry) => <div key={entry.sequence} className="border-l-2 pl-3"><p className="text-sm font-medium">{entry.kind.replaceAll('_', ' ')}</p><p className="mt-1 text-xs text-muted-foreground">{entry.actor || 'System'} · {new Date(entry.at).toLocaleString()}</p>{entry.response && <p className="mt-1 text-sm">{responseLabels[entry.response]}{entry.comment ? ` — ${entry.comment}` : ''}</p>}{entry.new_assignee && <p className="mt-1 text-xs text-muted-foreground">Assigned to {entry.new_assignee.id}</p>}</div>)}
              {history.data?.items.length === 0 && <p className="text-sm text-muted-foreground">No history is available.</p>}
              <nav className="flex items-center justify-between border-t pt-3" aria-label="Assignment history pages">
                <Button variant="outline" size="sm" disabled={history.isFetching || historyPage.previous.length === 0} onClick={() => updateHistoryPage(previousCursorPage)}>Earlier history</Button>
                <Button variant="outline" size="sm" disabled={history.isFetching || !history.data?.next_cursor} onClick={() => updateHistoryPage((current) => nextCursorPage(current, history.data?.next_cursor))}>Later history</Button>
              </nav>
            </CardContent></Card>
          </div>
        </>
      ) : null}
    </div>
  )
}
