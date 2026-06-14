import { Outlet } from '@tanstack/react-router'
import { Sidebar } from './Sidebar'
import { ChatWidget } from '@/components/chat/ChatWidget'
import { useUIStore } from '@/lib/ui-store'
import { Button } from '@/components/ui/button'
import { Menu, X } from 'lucide-react'
import { cn } from '@/lib/utils'

export function RootLayout() {
  const { sidebarOpen, toggleSidebar } = useUIStore()

  return (
    <div className="flex h-screen overflow-hidden bg-background">
      {/* Mobile overlay */}
      {sidebarOpen && (
        <div
          className="fixed inset-0 z-40 bg-black/50 md:hidden"
          onClick={toggleSidebar}
        />
      )}

      {/* Sidebar: fixed on mobile, static on desktop */}
      <div
        className={cn(
          'fixed inset-y-0 left-0 z-50 md:relative md:flex',
          sidebarOpen ? 'flex' : 'hidden md:flex',
        )}
      >
        <Sidebar />
      </div>

      {/* Main content */}
      <main className="flex-1 overflow-auto">
        {/* Mobile header bar */}
        <div className="flex h-12 items-center gap-3 border-b px-4 md:hidden">
          <Button variant="ghost" size="icon" onClick={toggleSidebar}>
            {sidebarOpen ? <X className="h-5 w-5" /> : <Menu className="h-5 w-5" />}
          </Button>
          <span className="font-semibold text-sm">Kora</span>
        </div>
        <Outlet />
      </main>

      {/* AI Chat Widget — floating button + panel */}
      <ChatWidget />
    </div>
  )
}
