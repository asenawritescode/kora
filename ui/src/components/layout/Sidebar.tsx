import { Link, useRouterState } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { fetchNavigation } from '@/lib/api/system'
import { useAuthStore } from '@/lib/auth-store'
import { useUIStore } from '@/lib/ui-store'
import { cn } from '@/lib/utils'
import { useState, useEffect } from 'react'
import {
  LayoutDashboard,
  LogOut,
  Moon,
  Sun,
  PanelLeftClose,
  PanelLeft,
  BookOpen,
  ChevronRight,
  Star,
  Clock,
} from 'lucide-react'
import { Button } from '@/components/ui/button'
import { ScrollArea } from '@/components/ui/scroll-area'
import { Separator } from '@/components/ui/separator'
import { Skeleton } from '@/components/ui/skeleton'
import { getFavorites, getRecentDoctypes, recordDoctypeVisit, toggleFavorite, isFavorite } from '@/lib/recent-doctypes'
import { LogoMark } from '@/components/ui/LogoMark'

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
        'flex items-center gap-3 rounded-lg px-3 py-2 text-sm font-medium transition-colors hover:bg-sidebar-accent hover:text-sidebar-accent-foreground whitespace-nowrap',
        isActive && 'bg-sidebar-accent text-sidebar-accent-foreground',
        collapsed && 'justify-center px-2',
      )}
      onClick={() => setSidebarOpen(false)}
    >
      {children}
    </Link>
  )
}

type FlyoutItem = {
  name: string; label: string; icon?: string
  to?: string // override link path
}

function FlyoutMenu({ label, items, collapsed, icon: Icon, isOpen, onOpen, onClose, onItemClick }: {
  label: string
  items: FlyoutItem[]
  collapsed: boolean
  icon?: typeof Star
  isOpen: boolean
  onOpen: () => void   // open this menu (close others)
  onClose: () => void  // close this menu (mouse leave)
  onItemClick?: (item: FlyoutItem) => void
}) {
  let closeTimer: ReturnType<typeof setTimeout> | null = null

  const handleMouseEnter = () => {
    if (closeTimer) { clearTimeout(closeTimer); closeTimer = null }
    onOpen()
  }
  const handleMouseLeave = () => {
    closeTimer = setTimeout(() => onClose(), 200)
  }
  // Click toggles: if open → close, if closed → open
  const handleClick = () => {
    if (closeTimer) { clearTimeout(closeTimer); closeTimer = null }
    isOpen ? onClose() : onOpen()
  }

  return (
    <div
      className="relative"
      onMouseEnter={handleMouseEnter}
      onMouseLeave={handleMouseLeave}
    >
      {/* Module trigger */}
      <button
        onClick={handleClick}
        className={cn(
          'flex w-full items-center gap-2 rounded-lg px-3 py-2 text-sm font-medium transition-colors hover:bg-sidebar-accent hover:text-sidebar-accent-foreground',
          collapsed && 'justify-center px-2',
        )}
      >
        {Icon && <Icon className="h-4 w-4 shrink-0" />}
        <span className="shrink-0 text-xs font-semibold uppercase tracking-wider text-muted-foreground truncate">
          {collapsed ? label.charAt(0) : label}
        </span>
        {!collapsed && <ChevronRight className={cn('h-3 w-3 ml-auto transition-transform', isOpen && 'rotate-90')} />}
      </button>

      {/* Flyout popout */}
      {isOpen && (
        <div className={cn(
          'z-50 bg-popover border rounded-lg shadow-lg py-1 min-w-[180px]',
          collapsed
            ? 'absolute left-full top-0 ml-1'
            : 'ml-2 mt-1',
        )}>
          <div className="px-2 py-1 text-[10px] font-semibold uppercase tracking-wider text-muted-foreground">
            {label}
          </div>
          {items.length === 0 && (
            <div className="px-3 py-2 text-xs text-muted-foreground italic">None yet</div>
          )}
          {items.map((item) => (
            <FlyoutRow key={item.name} item={item} onItemClick={onItemClick} />
          ))}
        </div>
      )}
    </div>
  )
}

export function Sidebar() {
  const { data, isLoading } = useQuery({
    queryKey: ['navigation'],
    queryFn: fetchNavigation,
    staleTime: 5 * 60_000,
  })

  const { user, logout } = useAuthStore()
  const { theme, toggleTheme, sidebarCollapsed, setSidebarCollapsed, setSidebarOpen } = useUIStore()

  // Accordion: only one menu open at a time — opening a menu closes others.
  const [openMenu, setOpenMenu] = useState<string | null>(null)
  const handleMenuOpen = (label: string) => setOpenMenu(label)
  const handleMenuClose = (label: string) => {
    setOpenMenu(prev => prev === label ? null : prev)
  }

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
        sidebarCollapsed ? 'w-16' : 'w-52',
      )}
    >
      {/* Header */}
      <div className="flex h-14 items-center justify-between px-4">
        {!sidebarCollapsed && (
          <span className="text-lg font-bold tracking-tight truncate flex items-center gap-2">
            <LogoMark size={20} />
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

      {/* Navigation + Admin — both scrollable */}
      <ScrollArea className="flex-1">
        <nav className="space-y-0.5 p-2">
          <NavItem to="/workspace" collapsed={sidebarCollapsed}>
            <LayoutDashboard className="h-4 w-4 shrink-0" />
            {!sidebarCollapsed && 'Home'}
          </NavItem>

          {isLoading && <NavSkeleton />}

          {/* Favorites */}
          <FavoritesFlyout collapsed={sidebarCollapsed} isOpen={openMenu === 'Favorites'} onOpen={() => handleMenuOpen('Favorites')} onClose={() => handleMenuClose('Favorites')} />

          {/* Recently Accessed */}
          <RecentFlyout collapsed={sidebarCollapsed} isOpen={openMenu === 'Recent'} onOpen={() => handleMenuOpen('Recent')} onClose={() => handleMenuClose('Recent')} />

          {data?.modules && data.modules.length > 0 && (
            <FlyoutMenu
              label="Modules"
              items={data.modules.flatMap(m => m.doctypes)}
              collapsed={sidebarCollapsed}
              isOpen={openMenu === 'Modules'}
              onOpen={() => handleMenuOpen('Modules')}
              onClose={() => handleMenuClose('Modules')}
              onItemClick={(item) => recordDoctypeVisit(item)}
            />
          )}

          <Separator className="my-2" />

          {/* Administrator flyout */}
          <FlyoutMenu
            label="Administrator"
            items={[
              { name: 'doctypes', label: 'DocTypes', to: '/workspace/admin/doctypes' },
              { name: 'permissions', label: 'Permissions', to: '/workspace/admin/permissions' },
              { name: 'workflows', label: 'Workflows', to: '/workspace/admin/workflows' },
              { name: 'versions', label: 'Versions', to: '/workspace/admin/versions' },
              { name: 'users', label: 'Users', to: '/workspace/admin/users' },
              { name: 'scripts', label: 'Scripts', to: '/workspace/admin/scripts' },
              { name: 'extensions', label: 'Extensions', to: '/workspace/admin/extensions' },
              { name: 'secrets', label: 'Secrets', to: '/workspace/admin/secrets' },
              { name: 'analytics', label: 'Analytics', to: '/workspace/admin/analytics' },
            ]}
            collapsed={sidebarCollapsed}
            isOpen={openMenu === 'Administrator'}
            onOpen={() => handleMenuOpen('Administrator')}
            onClose={() => handleMenuClose('Administrator')}
          />
          <PendingBadge />
          <a
            href="/api/v1/swagger-ui"
            target="_blank"
            rel="noopener noreferrer"
            onClick={() => setSidebarOpen(false)}
            className={cn(
              'flex items-center gap-3 rounded-lg px-3 py-2 text-sm font-medium transition-colors hover:bg-sidebar-accent hover:text-sidebar-accent-foreground',
              sidebarCollapsed && 'justify-center px-2',
            )}
          >
            <BookOpen className="h-4 w-4 shrink-0" />
            {!sidebarCollapsed && 'API Docs'}
          </a>
        </nav>
      </ScrollArea>

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

function FlyoutRow({ item, onItemClick }: { item: FlyoutItem; onItemClick?: (item: FlyoutItem) => void }) {
  const [fav, setFav] = useState(isFavorite(item.name))

  const handleStar = (e: React.MouseEvent) => {
    e.preventDefault()
    e.stopPropagation()
    const now = toggleFavorite(item)
    setFav(now)
    window.dispatchEvent(new Event('storage'))
  }

  return (
    <div className="flex items-center group/item">
      <Link
        to={(item.to || `/workspace/${encodeURIComponent(item.name)}`) as any}
        className="flex-1 flex items-center gap-2 px-3 py-1.5 text-sm hover:bg-accent hover:text-accent-foreground transition-colors whitespace-nowrap"
        onClick={() => onItemClick?.(item)}
      >
        <span className="shrink-0 text-base w-5 text-center">
          {item.icon || item.name.charAt(0).toUpperCase()}
        </span>
        {item.label}
      </Link>
      <button
        className="shrink-0 px-1.5 py-1 opacity-0 group-hover/item:opacity-100 transition-all"
        onClick={handleStar}
        title={fav ? 'Remove from favorites' : 'Add to favorites'}
      >
        <Star className={cn('h-3.5 w-3.5', fav && 'fill-amber-500 text-amber-500')} />
      </button>
    </div>
  )
}

function FavoritesFlyout({ collapsed, isOpen, onOpen, onClose }: { collapsed: boolean; isOpen: boolean; onOpen: () => void; onClose: () => void }) {
  const [favs, setFavs] = useState(getFavorites())

  // Re-render when localStorage changes (e.g., from another component)
  useEffect(() => {
    const onStorage = () => setFavs(getFavorites())
    window.addEventListener('storage', onStorage)
    return () => window.removeEventListener('storage', onStorage)
  }, [])

  return (
    <FlyoutMenu
      label="Favorites"
      icon={Star}
      items={favs}
      collapsed={collapsed}
      isOpen={isOpen}
      onOpen={onOpen}
      onClose={onClose}
      onItemClick={(item) => recordDoctypeVisit(item)}
    />
  )
}

function RecentFlyout({ collapsed, isOpen, onOpen, onClose }: { collapsed: boolean; isOpen: boolean; onOpen: () => void; onClose: () => void }) {
  const [recent, setRecent] = useState(getRecentDoctypes())

  useEffect(() => {
    const onStorage = () => setRecent(getRecentDoctypes())
    window.addEventListener('storage', onStorage)
    // Poll every 5s since storage events don't fire in same tab
    const interval = setInterval(() => setRecent(getRecentDoctypes()), 5000)
    return () => {
      window.removeEventListener('storage', onStorage)
      clearInterval(interval)
    }
  }, [])

  return (
    <FlyoutMenu
      label="Recent"
      icon={Clock}
      items={recent}
      collapsed={collapsed}
      isOpen={isOpen}
      onOpen={onOpen}
      onClose={onClose}
      onItemClick={(item) => recordDoctypeVisit(item)}
    />
  )
}

function PendingBadge() {
  const [draftCount, setDraftCount] = useState(0)

  useEffect(() => {
    fetch('/api/v1/system/config/versions?status=Draft')
      .then(r => r.json())
      .then(d => {
        if (d.data?.versions) {
          setDraftCount(d.data.versions.filter((v: any) => v.status === 'Draft').length)
        }
      })
      .catch(() => {})
  }, [])

  if (draftCount === 0) return null
  return (
    <span className="ml-auto rounded-full bg-amber-500 px-1.5 py-0.5 text-[10px] font-bold text-white">
      {draftCount}
    </span>
  )
}
