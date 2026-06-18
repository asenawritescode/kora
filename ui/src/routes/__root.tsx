import { createRootRoute, Outlet, useNavigate, useRouter } from '@tanstack/react-router'
import { useEffect } from 'react'
import { useAuthStore } from '@/lib/auth-store'
import { RootLayout } from '@/components/layout/RootLayout'

function AuthGuard() {
  const { isAuthenticated, isLoading, checkAuth } = useAuthStore()
  const navigate = useNavigate()
  const router = useRouter()
  const isConsole = window.location.pathname.startsWith('/console')

  useEffect(() => {
    // Console uses its own auth (Bearer token) — skip workspace session check.
    if (!isConsole) {
      checkAuth()
    }
  }, [])

  useEffect(() => {
    if (isConsole) return // Console has its own auth flow.
    if (!isLoading && !isAuthenticated) {
      const path = window.location.pathname
      if (!path.includes('/auth/login')) {
        navigate({ to: '/workspace/auth/login', search: { redirect: path } })
      }
    }
  }, [isAuthenticated, isLoading])

  // Console pages render immediately (their own AuthGuard handles redirect).
  if (isConsole) return <RootLayout />
  if (isLoading) return null
  if (!isAuthenticated) return null

  return <RootLayout />
}

export const Route = createRootRoute({
  component: AuthGuard,
})
