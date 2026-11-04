import { Link, useRouterState } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { fetchNavigation, navigationQueryKey } from '@/lib/api/system'
import { useAuthStore } from '@/lib/auth-store'
import { useUIStore } from '@/lib/ui-store'
import { cn } from '@/lib/utils'
import { useState, useEffect, useCallback, useMemo, useRef } from 'react'
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
  Boxes,
  Search,
  X,
  ClipboardCheck,
} from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { ScrollArea } from '@/components/ui/scroll-area'
import { Separator } from '@/components/ui/separator'
import { Skeleton } from '@/components/ui/skeleton'
import { getFavorites, getRecentDoctypes, recordDoctypeVisit, toggleFavorite, isFavorite } from '@/lib/recent-doctypes'
import { LogoMark } from '@/components/ui/LogoMark'
import type { ModuleGroup } from '@/types/api'

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
  onOpen: () => void
  onClose: () => void
  onItemClick?: (item: FlyoutItem) => void
}) {
  const closeTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null)
  const clickedRef = useRef(false)

  const handleMouseEnter = () => {
    if (closeTimerRef.current) { clearTimeout(closeTimerRef.current); closeTimerRef.current = null }
    onOpen()
  }
  const handleMouseLeave = () => {
    // Don't auto-close if the user clicked to open this menu.
    if (clickedRef.current) return
    closeTimerRef.current = setTimeout(() => {
      closeTimerRef.current = null
      onClose()
    }, 200)
  }
  const handleClick = () => {
    if (closeTimerRef.current) { clearTimeout(closeTimerRef.current); closeTimerRef.current = null }
    clickedRef.current = true
    onOpen()
  }

  // Reset clicked flag when menu closes.
  useEffect(() => {
    if (!isOpen) {
      clickedRef.current = false
    }
  }, [isOpen])

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

export function Sidebar({ mobile = false }: { mobile?: boolean }) {
  const { data, isLoading } = useQuery({
    queryKey: navigationQueryKey(),
    queryFn: fetchNavigation,
    staleTime: 5 * 60_000,
  })

  const { user, logout } = useAuthStore()
  const { theme, toggleTheme, sidebarCollapsed, setSidebarCollapsed, setSidebarOpen, activeModule, setActiveModule } = useUIStore()
  const [navQuery, setNavQuery] = useState('')
  const [favorites, setFavorites] = useState(getFavorites())
  const [recent, setRecent] = useState(getRecentDoctypes())
  const collapsed = mobile ? false : sidebarCollapsed

  // Accordion: only one menu open at a time — opening a menu closes others.
  const [openMenu, setOpenMenu] = useState<string | null>(null)
  const handleMenuOpen = (label: string) => setOpenMenu(label)
  const handleMenuClose = (label: string) => {
    setOpenMenu(prev => prev === label ? null : prev)
  }

  // Persist activeModule into openMenu on navigation changes only.
  useEffect(() => {
    if (activeModule && !openMenu) {
      setOpenMenu(`module:${activeModule}`)
    }
  }, [activeModule])

  useEffect(() => {
    const onStorage = () => {
      setFavorites(getFavorites())
      setRecent(getRecentDoctypes())
    }
    window.addEventListener('storage', onStorage)
    const interval = setInterval(onStorage, 5000)
    return () => {
      window.removeEventListener('storage', onStorage)
      clearInterval(interval)
    }
  }, [])

  const normalizedQuery = navQuery.trim().toLowerCase()
  const adminCapabilities = data?.admin_capabilities ?? []
  const allAdminItems = useMemo(() => ([
    { name: 'doctypes', label: 'DocTypes', to: '/workspace/admin/doctypes' },
    { name: 'permissions', label: 'Permissions', to: '/workspace/admin/permissions' },
    { name: 'automations', label: 'Automations', to: '/workspace/admin/automations' },
    { name: 'versions', label: 'Versions', to: '/workspace/admin/versions' },
    { name: 'users', label: 'Users', to: '/workspace/admin/users' },
    { name: 'scripts', label: 'Scripts', to: '/workspace/admin/scripts' },
    { name: 'extensions', label: 'Extensions', to: '/workspace/admin/extensions' },
    { name: 'secrets', label: 'Secrets', to: '/workspace/admin/secrets' },
    { name: 'analytics', label: 'Analytics', to: '/workspace/admin/analytics' },
    { name: 'page-manifests', label: 'Page manifests', to: '/workspace/admin/page-manifests' },
  ]), [])
  const adminItems = useMemo(() => {
    if (adminCapabilities.length === 0) return []
    return allAdminItems.filter((item) => adminCapabilities.includes(item.name))
  }, [allAdminItems, adminCapabilities])
  const filteredAdminItems = useMemo(() => {
    if (!normalizedQuery) return adminItems
    return adminItems.filter((item) =>
      item.label.toLowerCase().includes(normalizedQuery) ||
      item.name.toLowerCase().includes(normalizedQuery),
    )
  }, [adminItems, normalizedQuery])
  const filteredModules = useMemo(() => {
    if (!normalizedQuery) return data?.modules ?? []
    return (data?.modules ?? [])
      .map((module) => {
        const matchesModule = module.label.toLowerCase().includes(normalizedQuery) || module.module.toLowerCase().includes(normalizedQuery)
        const doctypes = module.doctypes.filter((doctype) =>
          doctype.label.toLowerCase().includes(normalizedQuery) ||
          doctype.name.toLowerCase().includes(normalizedQuery),
        )
        if (!matchesModule && doctypes.length === 0) return null
        return { ...module, doctypes: matchesModule ? module.doctypes : doctypes }
      })
      .filter(Boolean) as ModuleGroup[]
  }, [data?.modules, normalizedQuery])
  const filteredFavorites = useMemo(() => {
    if (!normalizedQuery) return favorites
    return favorites.filter((item) =>
      item.label.toLowerCase().includes(normalizedQuery) ||
      item.name.toLowerCase().includes(normalizedQuery),
    )
  }, [favorites, normalizedQuery])
  const filteredRecent = useMemo(() => {
    if (!normalizedQuery) return recent
    return recent.filter((item) =>
      item.label.toLowerCase().includes(normalizedQuery) ||
      item.name.toLowerCase().includes(normalizedQuery),
    )
  }, [recent, normalizedQuery])

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
        mobile ? 'w-full border-r-0' : sidebarCollapsed ? 'w-16' : 'w-52',
      )}
    >
      {/* Header */}
      <div className={cn('flex h-14 items-center justify-between px-4', mobile && 'pr-12')}>
        {!collapsed && (
          <span className="text-lg font-bold tracking-tight truncate flex items-center gap-2">
            <LogoMark size={20} />
            {data?.branding?.app_name || 'Kigae'}
          </span>
        )}
        {!mobile && (
          <Button
            variant="ghost"
            size="icon"
            onClick={() => setSidebarCollapsed(!sidebarCollapsed)}
            className="h-8 w-8 shrink-0"
          >
            {sidebarCollapsed ? <PanelLeft className="h-4 w-4" /> : <PanelLeftClose className="h-4 w-4" />}
          </Button>
        )}
      </div>
      <Separator />

      {/* Navigation + Admin — both scrollable */}
      <ScrollArea className="flex-1">
        <nav className="space-y-0.5 p-2">
          <NavItem to="/workspace" collapsed={collapsed}>
            <LayoutDashboard className="h-4 w-4 shrink-0" />
            {!collapsed && 'Home'}
          </NavItem>

          {data?.supported_capabilities?.includes('requests.inbox.v2') && (
            <NavItem to="/workspace/requests" collapsed={collapsed}>
              <ClipboardCheck className="h-4 w-4 shrink-0" />
              {!collapsed && 'Requests'}
            </NavItem>
          )}

          {!collapsed && (
            <div className="px-1 pb-2 pt-1">
              <div className="relative">
                <Search className="pointer-events-none absolute left-3 top-2.5 h-4 w-4 text-muted-foreground" />
                <Input
                  value={navQuery}
                  onChange={(e) => setNavQuery(e.target.value)}
                  placeholder="Search navigation..."
                  className="h-9 border-sidebar-border bg-sidebar pl-9 pr-9 text-sm text-sidebar-foreground placeholder:text-muted-foreground"
                />
                {navQuery.trim() && (
                  <button
                    type="button"
                    className="absolute right-1.5 top-1.5 rounded p-1 text-muted-foreground hover:bg-sidebar-accent hover:text-sidebar-accent-foreground"
                    onClick={() => setNavQuery('')}
                    aria-label="Clear navigation search"
                  >
                    <X className="h-4 w-4" />
                  </button>
                )}
              </div>
              {normalizedQuery && (
                <p className="mt-1 px-1 text-[11px] text-muted-foreground">
                  Filtering modules, favorites, recent items, and admin links.
                </p>
              )}
            </div>
          )}

          {isLoading && <NavSkeleton />}

          {/* Favorites */}
          {filteredFavorites.length > 0 && (
            <FavoritesFlyout
              collapsed={collapsed}
              items={filteredFavorites}
              isOpen={openMenu === 'Favorites'}
              onOpen={() => handleMenuOpen('Favorites')}
              onClose={() => handleMenuClose('Favorites')}
              onItemClick={() => setSidebarOpen(false)}
            />
          )}

          {/* Recently Accessed */}
          {filteredRecent.length > 0 && (
            <RecentFlyout
              collapsed={collapsed}
              items={filteredRecent}
              isOpen={openMenu === 'Recent'}
              onOpen={() => handleMenuOpen('Recent')}
              onClose={() => handleMenuClose('Recent')}
              onItemClick={() => setSidebarOpen(false)}
            />
          )}


          {filteredModules.length > 0 && (
            <div className="space-y-0.5">
              {!collapsed && (
                <div className="px-3 pb-1 pt-3 text-[10px] font-semibold uppercase tracking-wider text-muted-foreground">
                  Modules
                </div>
              )}
              {filteredModules.map((module) => (
                <ModuleSection
                  key={module.module}
                  module={module}
                  collapsed={collapsed}
                  isOpen={openMenu === `module:${module.module}`}
                  isActive={activeModule === module.module}
                  onOpen={() => handleMenuOpen(`module:${module.module}`)}
                  onClose={() => handleMenuClose(`module:${module.module}`)}
                  onItemClick={(item) => {
                    setSidebarOpen(false)
                    setActiveModule(module.module)
                    recordDoctypeVisit(item)
                  }}
                />
              ))}
            </div>
          )}

          {normalizedQuery && !filteredModules.length && !filteredFavorites.length && !filteredRecent.length && !filteredAdminItems.length && (
            <div className="px-3 py-4 text-center text-xs text-muted-foreground">
              No navigation matches.
            </div>
          )}

          <Separator className="my-2" />

          {/* Administrator flyout — only visible to users with the admin role. */}
          {filteredAdminItems.length > 0 && (
            <>
              <FlyoutMenu
                label="Administrator"
                items={filteredAdminItems}
                collapsed={collapsed}
                isOpen={openMenu === 'Administrator'}
                onOpen={() => handleMenuOpen('Administrator')}
                onClose={() => handleMenuClose('Administrator')}
                onItemClick={() => setSidebarOpen(false)}
              />
              <PendingBadge />
            </>
          )}
          <a
            href="/api/v1/swagger-ui"
            target="_blank"
            rel="noopener noreferrer"
            onClick={() => setSidebarOpen(false)}
            className={cn(
              'flex items-center gap-3 rounded-lg px-3 py-2 text-sm font-medium transition-colors hover:bg-sidebar-accent hover:text-sidebar-accent-foreground',
              collapsed && 'justify-center px-2',
            )}
          >
            <BookOpen className="h-4 w-4 shrink-0" />
            {!collapsed && 'API Docs'}
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
            collapsed && 'justify-center',
          )}
        >
          {theme === 'dark' ? <Sun className="h-4 w-4" /> : <Moon className="h-4 w-4" />}
          {!collapsed && (theme === 'dark' ? 'Light mode' : 'Dark mode')}
        </Button>

        {!collapsed && user && (
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
            collapsed && 'justify-center',
          )}
        >
          <LogOut className="h-4 w-4" />
          {!collapsed && 'Logout'}
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

function ModuleSection({
  module,
  collapsed,
  isOpen,
  isActive,
  onOpen,
  onClose,
  onItemClick,
}: {
  module: ModuleGroup
  collapsed: boolean
  isOpen: boolean
  isActive: boolean
  onOpen: () => void
  onClose: () => void
  onItemClick: (item: FlyoutItem) => void
}) {
  const items = module.doctypes.map((doctype) => ({
    name: doctype.name,
    label: doctype.label,
    icon: doctype.icon,
  }))

  if (collapsed) {
    return (
      <FlyoutMenu
        label={module.label}
        icon={Boxes}
        items={items}
        collapsed={collapsed}
        isOpen={isOpen}
        onOpen={onOpen}
        onClose={onClose}
        onItemClick={onItemClick}
      />
    )
  }

  return (
    <div className="rounded-lg">
      <button
        onClick={() => isOpen ? onClose() : onOpen()}
        className={cn(
          'flex w-full items-center gap-2 rounded-lg px-3 py-2 text-sm font-medium transition-colors hover:bg-sidebar-accent hover:text-sidebar-accent-foreground',
          isActive && 'bg-sidebar-accent/70 text-sidebar-accent-foreground',
        )}
      >
        <Boxes className="h-4 w-4 shrink-0" />
        <span className="min-w-0 flex-1 truncate text-left">{module.label}</span>
        <span className="rounded-full bg-sidebar-accent px-1.5 py-0.5 text-[10px] text-muted-foreground">
          {items.length}
        </span>
        <ChevronRight className={cn('h-3 w-3 transition-transform', isOpen && 'rotate-90')} />
      </button>

      {isOpen && (
        <div className="mt-1 space-y-0.5 border-l border-sidebar-border/70 pl-3 ml-5">
          {items.length === 0 && (
            <div className="px-3 py-2 text-xs italic text-muted-foreground">No doctypes yet</div>
          )}
          {items.map((item) => (
            <FlyoutRow key={item.name} item={item} onItemClick={onItemClick} />
          ))}
        </div>
      )}
    </div>
  )
}

function FavoritesFlyout({
  collapsed,
  items,
  isOpen,
  onOpen,
  onClose,
  onItemClick,
}: {
  collapsed: boolean
  items: FlyoutItem[]
  isOpen: boolean
  onOpen: () => void
  onClose: () => void
  onItemClick?: (item: FlyoutItem) => void
}) {
  return (
    <FlyoutMenu
      label="Favorites"
      icon={Star}
      items={items}
      collapsed={collapsed}
      isOpen={isOpen}
      onOpen={onOpen}
      onClose={onClose}
      onItemClick={(item) => {
        onItemClick?.(item)
        recordDoctypeVisit(item)
      }}
    />
  )
}

function RecentFlyout({
  collapsed,
  items,
  isOpen,
  onOpen,
  onClose,
  onItemClick,
}: {
  collapsed: boolean
  items: FlyoutItem[]
  isOpen: boolean
  onOpen: () => void
  onClose: () => void
  onItemClick?: (item: FlyoutItem) => void
}) {
  return (
    <FlyoutMenu
      label="Recent"
      icon={Clock}
      items={items}
      collapsed={collapsed}
      isOpen={isOpen}
      onOpen={onOpen}
      onClose={onClose}
      onItemClick={(item) => {
        onItemClick?.(item)
        recordDoctypeVisit(item)
      }}
    />
  )
}

function PendingBadge() {
  const [draftCount, setDraftCount] = useState(0)
  const { setSidebarOpen } = useUIStore()

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
    <Link
      to="/workspace/admin/versions"
      onClick={() => setSidebarOpen(false)}
      className="ml-auto block"
    >
      <span className="rounded-full bg-amber-500 px-1.5 py-0.5 text-[10px] font-bold text-white">
        {draftCount}
      </span>
    </Link>
  )
}
