import { createRouter, createRoute, createRootRoute, Outlet } from '@tanstack/react-router'
import { RootLayout } from '@/components/layout/RootLayout'
import { AuthGuard } from '@/components/layout/AuthGuard'
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

const routeTree = rootRoute.addChildren([
  loginRoute,
  workspaceLayout.addChildren([
    dashboardRoute,
    adminRoute.addChildren([
      adminDoctypesRoute,
      adminDoctypeNewRoute,
      adminDoctypeEditRoute,
      adminVersionsRoute,
      adminPermissionsRoute,
      adminWorkflowsRoute,
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
