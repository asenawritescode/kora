import { Component, StrictMode, type ErrorInfo, type ReactNode } from 'react'
import { createRoot } from 'react-dom/client'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { RouterProvider } from '@tanstack/react-router'
import { TooltipProvider } from '@/components/ui/tooltip'
import { router } from './router'
import { hashTenantQueryKey } from './lib/tenant-query-scope'
import './styles/index.css'

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      queryKeyHashFn: hashTenantQueryKey,
      staleTime: 5 * 60_000,           // 5 minutes — schemas and list data rarely change
      gcTime: 30 * 60_000,              // 30 minutes cache retention
      retry: 1,
      refetchOnWindowFocus: false,      // POS screens should not flicker on tab switch
      structuralSharing: true,          // Prevents object identity changes on refetch
    },
  },
})

interface ErrorBoundaryState {
  error: Error | null
}

class KoraErrorBoundary extends Component<{ children: ReactNode }, ErrorBoundaryState> {
  state: ErrorBoundaryState = { error: null }

  static getDerivedStateFromError(error: Error): ErrorBoundaryState {
    return { error }
  }

  componentDidCatch(error: Error, info: ErrorInfo) {
    console.error('Kigae failed to render', error, info)
  }

  render() {
    if (this.state.error) {
      return (
        <div className="flex min-h-svh items-center justify-center bg-background px-6 text-foreground">
          <div className="w-full max-w-md space-y-4 text-center">
            <h1 className="text-xl font-semibold">Kigae could not load this page</h1>
            <p className="text-sm text-muted-foreground">
              The page encountered a temporary loading error. Reload to try again.
            </p>
            <button
              type="button"
              className="rounded-md bg-primary px-4 py-2 text-sm font-medium text-primary-foreground"
              onClick={() => window.location.reload()}
            >
              Reload page
            </button>
          </div>
        </div>
      )
    }
    return this.props.children
  }
}

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <KoraErrorBoundary>
      <QueryClientProvider client={queryClient}>
        <TooltipProvider>
          <RouterProvider router={router} />
        </TooltipProvider>
      </QueryClientProvider>
    </KoraErrorBoundary>
  </StrictMode>,
)
