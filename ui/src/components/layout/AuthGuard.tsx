import { useEffect, type ReactNode } from 'react'
import { useAuthStore } from '@/lib/auth-store'
import { sitePath } from '@/lib/basepath'
import { Loader2 } from 'lucide-react'

interface AuthGuardProps {
  children: ReactNode
}

const PUBLIC_PATHS = ['/workspace/auth/login', '/console/login', '/console']

export function AuthGuard({ children }: AuthGuardProps) {
  const { isAuthenticated, isLoading, checkAuth } = useAuthStore()

  useEffect(() => {
    checkAuth()
  }, [])

  if (isLoading) {
    return (
      <div className="flex h-screen items-center justify-center bg-background">
        <Loader2 className="h-8 w-8 animate-spin text-muted-foreground" />
      </div>
    )
  }

  const currentPath = window.location.pathname

  // Check public paths against the path WITHOUT site prefix.
  // E.g., /s/fieldwork/workspace/auth/login → check /workspace/auth/login.
  const pathWithoutPrefix = currentPath.replace(/^\/s\/[^/]+/, '')
  const isPublic = PUBLIC_PATHS.some((p) => pathWithoutPrefix.startsWith(p))

  // Public paths: render children directly, no sidebar/layout.
  // Console uses its own auth system, so don't redirect console paths.
  if (isPublic) {
    const isConsolePath = pathWithoutPrefix.startsWith('/console')
    if (isAuthenticated && !isConsolePath) {
      window.location.href = sitePath('/workspace')
      return null
    }
    return <>{children}</>
  }

  // Protected paths: require auth.
  if (!isAuthenticated) {
    window.location.href = sitePath('/workspace/auth/login')
    return null
  }

  return <>{children}</>
}
