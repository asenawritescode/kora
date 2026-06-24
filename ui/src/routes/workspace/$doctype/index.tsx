import { useState } from 'react'
import { useParams, useNavigate } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { fetchDoctypeSchema } from '@/lib/api/system'
import { fetchList } from '@/lib/api/resources'
import { DataTable } from '@/components/tables/DataTable'
import { InsightsPanel } from '@/components/analytics/InsightsPanel'
import { Button } from '@/components/ui/button'
import { Skeleton } from '@/components/ui/skeleton'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { Breadcrumbs } from '@/components/layout/Breadcrumbs'
import { Plus, List, BarChart3 } from 'lucide-react'
import type { Document, DocType } from '@/types/kora'

export default function ListPage() {
  const { doctype } = useParams({ from: '/workspace/$doctype' })
  const navigate = useNavigate()

  const [page, setPage] = useState(0)
  const [sorting, setSorting] = useState<{ field: string; order: string } | null>(null)
  const [activeTab, setActiveTab] = useState<string>("list")
  const limit = 50

  const schemaQuery = useQuery({
    queryKey: ['doctype', doctype],
    queryFn: () => fetchDoctypeSchema(doctype),
    staleTime: 5 * 60_000,
  })

  const listQuery = useQuery({
    queryKey: ['resource', doctype, page, sorting],
    queryFn: () =>
      fetchList(doctype, {
        limit,
        offset: page * limit,
        order_by: sorting ? `${sorting.field} ${sorting.order}` : undefined,
      }),
    staleTime: 15_000,
    placeholderData: (prev) => prev,
  })

  const dt: DocType | undefined = schemaQuery.data?.doctype
  const perms = schemaQuery.data?.permissions
  const canCreate = perms?.create ?? false
  const canWrite = perms?.write ?? false
  const listFields = dt?.fields?.filter((f) => f.in_list_view && !isLayoutField(f.fieldtype)) ?? []
  const total = listQuery.data?.meta?.total ?? 0
  const totalPages = Math.ceil(total / limit)

  if (schemaQuery.isLoading || listQuery.isLoading) {
    return (
      <div className="p-8 space-y-4">
        <Skeleton className="h-8 w-48" />
        <Skeleton className="h-64 w-full" />
      </div>
    )
  }

  if (schemaQuery.isError || !dt) {
    return (
      <div className="flex h-64 items-center justify-center">
        <p className="text-muted-foreground">DocType "{doctype}" not found.</p>
      </div>
    )
  }

  return (
    <div className="p-4 md:p-8">
      <Breadcrumbs items={[{ label: dt.name }]} className="mb-2" />
      {/* Header */}
      <div className="mb-6 flex items-center justify-between flex-wrap gap-3">
        <div>
          <h1 className="text-2xl font-bold tracking-tight">{dt.name}</h1>
          <p className="text-sm text-muted-foreground">
            {total} record{total !== 1 ? 's' : ''}
            {!canWrite && <span className="ml-2 text-amber-600 dark:text-amber-400">(read-only)</span>}
          </p>
        </div>
        <Button
          onClick={() => navigate({ to: '/workspace/$doctype/new', params: { doctype } })}
          disabled={!canCreate}
          title={!canCreate ? "You don't have permission to create" : undefined}
        >
          <Plus className="mr-2 h-4 w-4" />
          New {dt.name}
        </Button>
      </div>

      {/* Tab bar: List | Insights */}
      <Tabs value={activeTab} onValueChange={setActiveTab} className="w-full">
        <TabsList className="mb-6">
          <TabsTrigger value="list" className="flex items-center gap-2">
            <List className="h-4 w-4" />
            List
          </TabsTrigger>
          <TabsTrigger value="insights" className="flex items-center gap-2">
            <BarChart3 className="h-4 w-4" />
            Insights
          </TabsTrigger>
        </TabsList>

        <TabsContent value="list">
          <DataTable
            columns={listFields}
            data={(listQuery.data?.data as Document[]) ?? []}
            titleField={dt.title_field}
            total={total}
            page={page}
            totalPages={totalPages}
            sorting={sorting}
            onSortingChange={setSorting}
            onPageChange={setPage}
            onRowClick={(doc) =>
              navigate({
                to: '/workspace/$doctype/$name',
                params: { doctype, name: doc.name },
              })
            }
            isEmpty={!listQuery.isFetching && total === 0}
            isFetching={listQuery.isFetching}
            isError={listQuery.isError}
            onRetry={() => listQuery.refetch()}
          />
        </TabsContent>

        <TabsContent value="insights">
          <InsightsPanel doctype={doctype} />
        </TabsContent>
      </Tabs>
    </div>
  )
}

function isLayoutField(fieldtype: string): boolean {
  return ['Section Break', 'Column Break', 'Heading', 'Table'].includes(fieldtype)
}
