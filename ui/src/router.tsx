import { useEffect, Suspense, lazy } from 'react'
import { createRouter, createRoute, createRootRoute, Outlet, Navigate } from '@tanstack/react-router'
import { RootLayout } from '@/components/layout/RootLayout'
import { ConsoleLayout } from '@/components/layout/ConsoleLayout'
import { AuthGuard } from '@/components/layout/AuthGuard'
import { useConsoleAuthStore } from '@/lib/console-auth-store'
import { Loader2 } from 'lucide-react'

const LoginPage = lazy(() => import('@/routes/workspace/auth/login'))
const DashboardPage = lazy(() => import('@/routes/workspace/index'))
const ListPage = lazy(() => import('@/routes/workspace/$doctype/index'))
const NewFormPage = lazy(() => import('@/routes/workspace/$doctype/new'))
const EditFormPage = lazy(() => import('@/routes/workspace/$doctype/$name'))
const AdminDoctypesPage = lazy(() => import('@/routes/workspace/admin/doctypes'))
const AdminDoctypeEditorPage = lazy(() => import('@/routes/workspace/admin/doctypes/editor'))
const AdminVersionsPage = lazy(() => import('@/routes/workspace/admin/versions'))
const AdminPermissionsPage = lazy(() => import('@/routes/workspace/admin/permissions'))
const AdminWorkflowsPage = lazy(() => import('@/routes/workspace/admin/workflows'))
const AdminUsersPage = lazy(() => import('@/routes/workspace/admin/users'))
const AdminSecretsPage = lazy(() => import('@/routes/workspace/admin/secrets'))
const AdminScriptsPage = lazy(() => import('@/routes/workspace/admin/scripts'))
const AdminExtensionsPage = lazy(() => import('@/routes/workspace/admin/extensions'))
const AdminAnalyticsPage = lazy(() => import('@/routes/workspace/admin/analytics'))
const ConsoleLoginPage = lazy(() => import('@/routes/console/login'))
const ConsoleDashboard = lazy(() => import('@/routes/console/index'))

// Root — just auth guard, no layout.
const rootRoute = createRootRoute({
  component: () => (
    <AuthGuard>
      <Suspense fallback={<div className="flex items-center justify-center h-screen"><Loader2 className="h-8 w-8 animate-spin" /></div>}>
        <Outlet />
      </Suspense>
    </AuthGuard>
  ),
})

// Login route at root level — no sidebar. Public.
const loginRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/workspace/auth/login',
  component: LoginPage,
})

// Workspace layout with sidebar — all authenticated pages are nested here.
const workspaceLayout = createRoute({
  getParentRoute: () => rootRoute,
  path: '/workspace',
  component: () => <RootLayout />,
})

// Dashboard at /workspace.
const dashboardRoute = createRoute({
  getParentRoute: () => workspaceLayout,
  path: '/',
  component: DashboardPage,
})

// Doctype CRUD routes.
const doctypeRoute = createRoute({
  getParentRoute: () => workspaceLayout,
  path: '$doctype',
})

const doctypeListRoute = createRoute({
  getParentRoute: () => doctypeRoute,
  path: '/',
  component: ListPage,
})

const doctypeNewRoute = createRoute({
  getParentRoute: () => doctypeRoute,
  path: 'new',
  component: NewFormPage,
})

const doctypeEditRoute = createRoute({
  getParentRoute: () => doctypeRoute,
  path: '$name',
  component: EditFormPage,
})

// Admin routes.
const adminRoute = createRoute({
  getParentRoute: () => workspaceLayout,
  path: 'admin',
})

const adminDoctypesRoute = createRoute({
  getParentRoute: () => adminRoute,
  path: 'doctypes',
  component: AdminDoctypesPage,
})

const adminDoctypeNewRoute = createRoute({
  getParentRoute: () => adminRoute,
  path: 'doctypes/new',
  component: AdminDoctypeEditorPage,
})

const adminDoctypeEditRoute = createRoute({
  getParentRoute: () => adminRoute,
  path: 'doctypes/$name',
  component: AdminDoctypeEditorPage,
})

const adminVersionsRoute = createRoute({
  getParentRoute: () => adminRoute,
  path: 'versions',
  component: AdminVersionsPage,
})

const adminPermissionsRoute = createRoute({
  getParentRoute: () => adminRoute,
  path: 'permissions',
  component: AdminPermissionsPage,
})

const adminWorkflowsRoute = createRoute({
  getParentRoute: () => adminRoute,
  path: 'workflows',
  component: AdminWorkflowsPage,
})

const adminUsersRoute = createRoute({
  getParentRoute: () => adminRoute,
  path: 'users',
  component: AdminUsersPage,
})

const adminScriptsRoute = createRoute({
  getParentRoute: () => adminRoute,
  path: 'scripts',
  component: AdminScriptsPage,
})

const adminExtensionsRoute = createRoute({
  getParentRoute: () => adminRoute,
  path: 'extensions',
  component: AdminExtensionsPage,
})

const adminSecretsRoute = createRoute({
  getParentRoute: () => adminRoute,
  path: 'secrets',
  component: AdminSecretsPage,
})

const adminAnalyticsRoute = createRoute({
  getParentRoute: () => adminRoute,
  path: 'analytics',
  component: AdminAnalyticsPage,
})

// Settings (placeholder).
const settingsRoute = createRoute({
  getParentRoute: () => workspaceLayout,
  path: 'settings',
  component: () => (
    <div className="p-8">
      <h1 className="text-2xl font-bold">Settings</h1>
      <p className="mt-2 text-muted-foreground">Workspace settings coming soon.</p>
    </div>
  ),
})

// Console login — public.
const consoleLoginRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/console/login',
  component: ConsoleLoginPage,
})

// Console layout — protected by token auth, renders ConsoleLayout shell.
const consoleLayout = createRoute({
  getParentRoute: () => rootRoute,
  path: '/console',
  component: () => {
    const { isAuthenticated, isLoading, checkAuth } = useConsoleAuthStore()
    useEffect(() => { checkAuth() }, [])
    if (isLoading) return <div className="flex min-h-screen items-center justify-center"><Loader2 className="h-8 w-8 animate-spin text-muted-foreground" /></div>
    if (!isAuthenticated) return <Navigate to="/console/login" />
    return <ConsoleLayout />
  },
})

const consoleIndexRoute = createRoute({
  getParentRoute: () => consoleLayout,
  path: '/',
  component: ConsoleDashboard,
})

const routeTree = rootRoute.addChildren([
  loginRoute,
  consoleLoginRoute,
  consoleLayout.addChildren([
    consoleIndexRoute,
  ]),
  workspaceLayout.addChildren([
    dashboardRoute,
    adminRoute.addChildren([
      adminDoctypesRoute,
      adminDoctypeNewRoute,
      adminDoctypeEditRoute,
      adminVersionsRoute,
      adminPermissionsRoute,
      adminWorkflowsRoute,
      adminUsersRoute,
      adminScriptsRoute,
      adminExtensionsRoute,
      adminSecretsRoute,
      adminAnalyticsRoute,
    ]),
    doctypeRoute.addChildren([doctypeListRoute, doctypeNewRoute, doctypeEditRoute]),
    settingsRoute,
  ]),
])

// Auto-detect basepath for path-based site URLs (/s/:site).
function getBasepath(): string {
  const m = window.location.pathname.match(/^(\/s\/[^/]+)/)
  return m ? m[1] : ''
}

export const router = createRouter({
  routeTree,
  basepath: getBasepath(),
  defaultPreload: 'intent',
})

declare module '@tanstack/react-router' {
  interface Register {
    router: typeof router
  }
}
