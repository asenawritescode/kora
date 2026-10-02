import { Outlet, useRouterState } from '@tanstack/react-router'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Sidebar } from './Sidebar'
import { ChatWidget } from '@/components/chat/ChatWidget'
import { useUIStore } from '@/lib/ui-store'
import { fetchNavigation } from '@/lib/api/system'
import { Button } from '@/components/ui/button'
import { Menu, X, Bell, ExternalLink } from 'lucide-react'
import { ToastContainer } from '@/components/ui/Toast'
import { Badge } from '@/components/ui/badge'
import { Suspense, useEffect, useMemo, useRef, useState } from 'react'
import { Sheet, SheetContent } from '@/components/ui/sheet'
import { loadRuntimeConfig } from '@/lib/runtime-config'
import { attachRealtimeInvalidation, type RealtimeEvent } from '@/lib/realtime'
import type { RealtimeConnectionState } from '@/types/api'
import { toast } from '@/components/ui/Toast'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { formatRealtimeNotificationMessage, formatRealtimeNotificationTitle, getRootRealtimeBadge } from './root-layout-helpers'
import { prependNotification, subscribeToRealtimeNotifications } from './notifications'

function WorkspaceRouteFallback() {
  return (
    <div className="flex min-h-full items-center justify-center bg-background px-6">
      <div className="w-full max-w-sm rounded-2xl border bg-card px-6 py-8 text-center shadow-sm">
        <div className="mx-auto h-7 w-7 animate-spin rounded-full border-2 border-muted border-t-foreground" aria-hidden="true" />
        <div className="mt-3" role="status" aria-live="polite">
          <p className="text-sm font-medium">Loading page</p>
          <p className="mt-1 text-sm text-muted-foreground">Preparing the workspace view.</p>
        </div>
      </div>
    </div>
  )
}

export function RootLayout() {
  const { sidebarOpen, toggleSidebar, setSidebarOpen, setActiveModule, setShellMode } = useUIStore()
  const routerState = useRouterState()
  const pathname = routerState.location.pathname
  const runtime = useMemo(() => loadRuntimeConfig(), [])
  const [online, setOnline] = useState(typeof navigator === 'undefined' ? true : navigator.onLine)
  const [realtime, setRealtime] = useState<RealtimeConnectionState>({ state: runtime.realtimeBaseUrl ? 'connecting' : 'offline' })
  const [notifications, setNotifications] = useState<RealtimeEvent[]>([])
  const [notificationsOpen, setNotificationsOpen] = useState(false)
  const lastRealtimeState = useRef<RealtimeConnectionState['state']>(realtime.state)
  const queryClient = useQueryClient()

  const { data: navigation } = useQuery({
    queryKey: ['navigation'],
    queryFn: fetchNavigation,
    staleTime: 5 * 60_000,
  })

  useEffect(() => {
    if (!pathname.startsWith('/workspace')) return

    if (pathname.startsWith('/workspace/admin')) {
      setShellMode('advanced')
      setActiveModule(null)
      return
    }

    setShellMode('simple')

    const [, , doctype] = pathname.split('/')
    if (!doctype || !navigation?.modules) {
      setActiveModule(null)
      return
    }

    const decodedDoctype = decodeURIComponent(doctype)
    const module = navigation.modules.find((group) =>
      group.doctypes.some((item) => item.name === decodedDoctype),
    )
    setActiveModule(module?.module ?? null)
  }, [navigation?.modules, pathname, setActiveModule, setShellMode])

  useEffect(() => attachRealtimeInvalidation(runtime.realtimeBaseUrl, queryClient, setRealtime), [runtime.realtimeBaseUrl, queryClient])

  useEffect(() => {
    if (realtime.state !== 'connected' || lastRealtimeState.current === 'connected') {
      lastRealtimeState.current = realtime.state
      return
    }
    lastRealtimeState.current = realtime.state
    void queryClient.invalidateQueries()
  }, [queryClient, realtime.state])

  useEffect(() => {
    return subscribeToRealtimeNotifications((detail) => {
      setNotifications((current) => {
        return prependNotification(current, detail)
      })
      const message = detail?.message || detail?.title
      if (message) {
        toast((detail.severity as any) || 'info', message)
      }
    })
  }, [])

  useEffect(() => {
    const onOnline = () => setOnline(true)
    const onOffline = () => setOnline(false)
    window.addEventListener('online', onOnline)
    window.addEventListener('offline', onOffline)
    return () => {
      window.removeEventListener('online', onOnline)
      window.removeEventListener('offline', onOffline)
    }
  }, [])

  return (
    <div className="flex h-screen overflow-hidden bg-background">
      <div className="hidden md:flex md:relative md:z-auto">
        <Sidebar />
      </div>

      <Sheet key={sidebarOpen ? 'open' : 'closed'} open={sidebarOpen} onOpenChange={setSidebarOpen}>
        <SheetContent side="left" className="w-[86vw] max-w-sm p-0 md:hidden">
          <Sidebar mobile />
        </SheetContent>
      </Sheet>

      {/* Main content */}
      <main className="flex-1 overflow-auto">
        <div className="hidden md:flex items-center justify-end gap-2 border-b px-4 py-2">
          <Badge variant={online ? 'outline' : 'destructive'}>
            {online ? 'Online' : 'Offline'}
          </Badge>
          <Badge variant={getRootRealtimeBadge(realtime).variant}>
            {getRootRealtimeBadge(realtime).label}
          </Badge>
          {realtime.state !== 'connected' && (
            <span className="max-w-72 truncate text-xs text-muted-foreground">
              {getRootRealtimeBadge(realtime).detail}
            </span>
          )}
          <Button variant="ghost" size="icon" className="h-8 w-8" aria-label="Notifications" onClick={() => setNotificationsOpen((v) => !v)}>
            <Bell className="h-4 w-4" />
          </Button>
          {runtime.capabilitiesVersion && (
            <Badge variant="secondary">
              Capabilities {runtime.capabilitiesVersion}
            </Badge>
          )}
        </div>
        {/* Mobile header bar */}
        <div className="flex h-12 items-center gap-3 border-b px-4 md:hidden">
          <Button variant="ghost" size="icon" aria-label={sidebarOpen ? 'Close navigation' : 'Open navigation'} onClick={toggleSidebar}>
            {sidebarOpen ? <X className="h-5 w-5" /> : <Menu className="h-5 w-5" />}
          </Button>
          <span className="font-semibold text-sm">Kora</span>
          <div className="ml-auto flex items-center gap-2">
            <Badge variant={online ? 'outline' : 'destructive'}>
              {online ? 'Online' : 'Offline'}
            </Badge>
            <Badge variant={getRootRealtimeBadge(realtime).variant}>
              {getRootRealtimeBadge(realtime).label}
            </Badge>
            {realtime.state !== 'connected' && (
              <span className="max-w-56 truncate text-[11px] text-muted-foreground">
                {getRootRealtimeBadge(realtime).detail}
              </span>
            )}
            <Button variant="ghost" size="icon" className="h-7 w-7" aria-label="Notifications" onClick={() => setNotificationsOpen((v) => !v)}>
              <Bell className="h-4 w-4" />
            </Button>
            {runtime.capabilitiesVersion && (
              <Badge variant="secondary">
                Capabilities {runtime.capabilitiesVersion}
              </Badge>
            )}
          </div>
        </div>
        <Suspense fallback={<WorkspaceRouteFallback />}>
          <Outlet />
        </Suspense>
      </main>

      {/* AI Chat Widget — floating button + panel */}
      <ChatWidget />

      {/* Toast notifications */}
      <ToastContainer />

      {notificationsOpen && (
        <div className="fixed right-4 top-16 z-40 w-[22rem] max-w-[calc(100vw-2rem)]">
          <Card className="shadow-xl">
            <CardHeader className="flex flex-row items-center justify-between space-y-0">
              <CardTitle className="text-sm">Notifications</CardTitle>
              <Button variant="ghost" size="sm" onClick={() => setNotifications([])}>Clear</Button>
            </CardHeader>
            <CardContent className="space-y-2">
              {notifications.length === 0 ? (
                <p className="text-sm text-muted-foreground">No recent updates.</p>
              ) : notifications.map((item, index) => (
                <div key={`${item.type}-${item.occurred_at ?? index}-${item.doc_name ?? index}`} className="rounded-lg border p-3 text-sm">
                  <div className="font-medium">{formatRealtimeNotificationTitle(item)}</div>
                  <div className="text-muted-foreground">{formatRealtimeNotificationMessage(item)}</div>
                  <div className="mt-2 flex items-center justify-between gap-2">
                    <Badge variant="outline">{item.severity || 'info'}</Badge>
                    {item.action?.href && (
                      <a className="inline-flex items-center gap-1 text-xs text-primary hover:underline" href={item.action.href}>
                        {item.action.label || 'Open'}
                        <ExternalLink className="h-3 w-3" />
                      </a>
                    )}
                  </div>
                </div>
              ))}
            </CardContent>
          </Card>
        </div>
      )}
    </div>
  )
}
