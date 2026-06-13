import { useQuery } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { fetchNavigation } from '@/lib/api/system'
import { useAuthStore } from '@/lib/auth-store'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Skeleton } from '@/components/ui/skeleton'
import { LayoutGrid, ArrowRight } from 'lucide-react'

export default function DashboardPage() {
  const { data, isLoading } = useQuery({
    queryKey: ['navigation'],
    queryFn: fetchNavigation,
    staleTime: 5 * 60_000,
  })

  const { user } = useAuthStore()

  return (
    <div className="p-8">
      {/* Welcome */}
      <div className="mb-8">
        <h1 className="text-3xl font-bold tracking-tight">
          Welcome{user?.full_name ? `, ${user.full_name}` : ''}
        </h1>
        <p className="mt-1 text-muted-foreground">
          {user?.roles?.length ? `Role: ${user.roles.join(', ')}` : 'Dashboard'}
        </p>
      </div>

      {/* Module cards */}
      {isLoading ? (
        <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
          {[1, 2, 3].map((i) => (
            <Skeleton key={i} className="h-32" />
          ))}
        </div>
      ) : (
        <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
          {data?.modules?.map((mod) =>
            mod.doctypes.map((dt) => (
              <Link
                key={dt.name}
                to="/workspace/$doctype"
                params={{ doctype: dt.name }}
                className="group"
              >
                <Card className="h-full transition-shadow hover:shadow-md">
                  <CardHeader>
                    <CardTitle className="flex items-center justify-between text-lg">
                      <span>{dt.label}</span>
                      <ArrowRight className="h-4 w-4 text-muted-foreground opacity-0 transition-opacity group-hover:opacity-100" />
                    </CardTitle>
                  </CardHeader>
                  <CardContent>
                    <p className="text-sm text-muted-foreground">
                      Module: {mod.label}
                    </p>
                  </CardContent>
                </Card>
              </Link>
            )),
          )}
        </div>
      )}

      {!isLoading && !data?.modules?.length && (
        <div className="rounded-lg border border-dashed p-12 text-center">
          <LayoutGrid className="mx-auto h-12 w-12 text-muted-foreground" />
          <h3 className="mt-4 text-lg font-medium">No modules configured</h3>
          <p className="mt-2 text-sm text-muted-foreground">
            Run <code className="rounded bg-muted px-1 py-0.5 text-xs">kora config import</code> to load doctypes.
          </p>
        </div>
      )}
    </div>
  )
}
