import { useEffect } from 'react'
import { createRouter, createRoute, createRootRoute, Outlet, Navigate } from '@tanstack/react-router'
import { RootLayout } from '@/components/layout/RootLayout'
import { ConsoleLayout } from '@/components/layout/ConsoleLayout'
import { AuthGuard } from '@/components/layout/AuthGuard'
import { useConsoleAuthStore } from '@/lib/console-auth-store'
import { Loader2 } from 'lucide-react'
import LoginPage from '@/routes/workspace/auth/login'
import DashboardPage from '@/routes/workspace/index'
import ListPage from '@/routes/workspace/$doctype/index'
import NewFormPage from '@/routes/workspace/$doctype/new'
import EditFormPage from '@/routes/workspace/$doctype/$name'
import AdminDoctypesPage from '@/routes/workspace/admin/doctypes'
import AdminDoctypeEditorPage from '@/routes/workspace/admin/doctypes/editor'
import AdminVersionsPage from '@/routes/workspace/admin/versions'
import AdminPermissionsPage from '@/routes/workspace/admin/permissions'
import AdminWorkflowsPage from '@/routes/workspace/admin/workflows'
import AdminUsersPage from '@/routes/workspace/admin/users'
import AdminSecretsPage from '@/routes/workspace/admin/secrets'
import AdminAnalyticsPage from '@/routes/workspace/admin/analytics'
import ConsoleLoginPage from '@/routes/console/login'
import ConsoleDashboard from '@/routes/console/index'
import MarketingLayout from '@/components/landing/MarketingLayout'
import DocsLayout from '@/components/landing/DocsLayout'
import HomePage from '@/routes/landing/home'
import ExamplesPage from '@/routes/landing/examples'
import DocsPage from '@/routes/landing/docs'
import BlogPage from '@/routes/landing/blog'

// Root — just auth guard, no layout.
const rootRoute = createRootRoute({
  component: () => (
    <AuthGuard>
      <Outlet />
    </AuthGuard>
  ),
})

// Login route at root level — no sidebar. Public.
const loginRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/workspace/auth/login',
  component: LoginPage,
})

// Marketing pages — public, direct children of rootRoute.
// Each wraps its own layout to avoid path nesting conflicts.

const homeRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/',
  component: () => <MarketingLayout><HomePage /></MarketingLayout>,
})

const examplesRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/examples',
  component: () => <MarketingLayout><ExamplesPage /></MarketingLayout>,
})

const blogRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/blog',
  component: () => <MarketingLayout><BlogPage /></MarketingLayout>,
})

// Docs gets its own layout (nav + sidebar + content).
const docsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/docs',
  component: () => <DocsLayout><DocsPage /></DocsLayout>,
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
  homeRoute,
  examplesRoute,
  blogRoute,
  docsRoute,
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
