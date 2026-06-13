import { Link, useRouterState } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { fetchNavigation } from '@/lib/api/system'
import { useAuthStore } from '@/lib/auth-store'
import { useUIStore } from '@/lib/ui-store'
import { cn } from '@/lib/utils'
import {
  LayoutDashboard,
  LogOut,
  Moon,
  Sun,
  PanelLeftClose,
  PanelLeft,
  FileCode,
  History,
  ShieldCheck,
  Workflow,
} from 'lucide-react'
import { Button } from '@/components/ui/button'
import { ScrollArea } from '@/components/ui/scroll-area'
import { Separator } from '@/components/ui/separator'
import { Skeleton } from '@/components/ui/skeleton'

function NavItem({
  to,
  params,
  children,
  collapsed,
}: {
  to: string
  params?: Record<string, string>
  children: React.ReactNode
  collapsed: boolean
}) {
  const routerState = useRouterState()
  const { setSidebarOpen } = useUIStore()
  const isActive = routerState.location.pathname === to ||
    (params && routerState.location.pathname.startsWith(to.replace(/\/$/, '')))

  return (
    <Link
      to={to as any}
      params={params as any}
      className={cn(
        'flex items-center gap-3 rounded-lg px-3 py-2 text-sm font-medium transition-colors hover:bg-sidebar-accent hover:text-sidebar-accent-foreground',
        isActive && 'bg-sidebar-accent text-sidebar-accent-foreground',
        collapsed && 'justify-center px-2',
      )}
      onClick={() => setSidebarOpen(false)}
    >
      {children}
    </Link>
  )
}

export function Sidebar() {
  const { data, isLoading } = useQuery({
    queryKey: ['navigation'],
    queryFn: fetchNavigation,
    staleTime: 5 * 60_000,
  })

  const { user, logout } = useAuthStore()
  const { theme, toggleTheme, sidebarCollapsed, setSidebarCollapsed } = useUIStore()

  const NavSkeleton = () => (
    <div className="space-y-2 p-4">
      <Skeleton className="h-5 w-24" />
      {[1, 2, 3].map((i) => (
        <Skeleton key={i} className="h-8 w-full" />
      ))}
    </div>
  )

  return (
    <aside
      className={cn(
        'flex h-full flex-col border-r bg-sidebar text-sidebar-foreground transition-all duration-200',
        sidebarCollapsed ? 'w-16' : 'w-60',
      )}
    >
      {/* Header */}
      <div className="flex h-14 items-center justify-between px-4">
        {!sidebarCollapsed && (
          <span className="text-lg font-bold tracking-tight">
            {data?.branding?.app_name || 'Kora'}
          </span>
        )}
        <Button
          variant="ghost"
          size="icon"
          onClick={() => setSidebarCollapsed(!sidebarCollapsed)}
          className="h-8 w-8 shrink-0"
        >
          {sidebarCollapsed ? <PanelLeft className="h-4 w-4" /> : <PanelLeftClose className="h-4 w-4" />}
        </Button>
      </div>
      <Separator />

      {/* Navigation */}
      <ScrollArea className="flex-1">
        <nav className="space-y-1 p-2">
          <NavItem to="/workspace" collapsed={sidebarCollapsed}>
            <LayoutDashboard className="h-4 w-4 shrink-0" />
            {!sidebarCollapsed && 'Home'}
          </NavItem>

          {isLoading && <NavSkeleton />}

          {data?.modules?.map((mod) => (
            <div key={mod.module} className="pt-2">
              {!sidebarCollapsed && (
                <h4 className="mb-1 px-3 text-xs font-semibold uppercase tracking-wider text-muted-foreground">
                  {mod.label}
                </h4>
              )}
              {mod.doctypes.map((dt) => (
                <NavItem
                  key={dt.name}
                  to={`/workspace/${encodeURIComponent(dt.name)}`}
                  collapsed={sidebarCollapsed}
                >
                  <span className="shrink-0 text-base">
                    {dt.icon || dt.name.charAt(0).toUpperCase()}
                  </span>
                  {!sidebarCollapsed && dt.label}
                </NavItem>
              ))}
            </div>
          ))}
        </nav>
      </ScrollArea>

      <Separator />

      {/* Administrator section */}
      <nav className="space-y-1 p-2">
        {!sidebarCollapsed && (
          <h4 className="mb-1 px-3 text-xs font-semibold uppercase tracking-wider text-muted-foreground">
            Administrator
          </h4>
        )}
        <NavItem to="/workspace/admin/doctypes" collapsed={sidebarCollapsed}>
          <FileCode className="h-4 w-4 shrink-0" />
          {!sidebarCollapsed && 'DocTypes'}
        </NavItem>
        <NavItem to="/workspace/admin/permissions" collapsed={sidebarCollapsed}>
          <ShieldCheck className="h-4 w-4 shrink-0" />
          {!sidebarCollapsed && 'Permissions'}
        </NavItem>
        <NavItem to="/workspace/admin/workflows" collapsed={sidebarCollapsed}>
          <Workflow className="h-4 w-4 shrink-0" />
          {!sidebarCollapsed && 'Workflows'}
        </NavItem>
        <NavItem to="/workspace/admin/versions" collapsed={sidebarCollapsed}>
          <History className="h-4 w-4 shrink-0" />
          {!sidebarCollapsed && 'Versions'}
        </NavItem>
      </nav>

      <Separator />

      {/* Footer */}
      <div className="p-2 space-y-1">
        <Button
          variant="ghost"
          size="sm"
          onClick={toggleTheme}
          className={cn(
            'w-full justify-start gap-2 text-xs',
            sidebarCollapsed && 'justify-center',
          )}
        >
          {theme === 'dark' ? <Sun className="h-4 w-4" /> : <Moon className="h-4 w-4" />}
          {!sidebarCollapsed && (theme === 'dark' ? 'Light mode' : 'Dark mode')}
        </Button>

        {!sidebarCollapsed && user && (
          <p className="px-3 text-xs text-muted-foreground truncate" title={user.email}>
            {user.full_name || user.name}
          </p>
        )}

        <Button
          variant="ghost"
          size="sm"
          onClick={logout}
          className={cn(
            'w-full justify-start gap-2 text-xs text-muted-foreground',
            sidebarCollapsed && 'justify-center',
          )}
        >
          <LogOut className="h-4 w-4" />
          {!sidebarCollapsed && 'Logout'}
        </Button>
      </div>
    </aside>
  )
}
