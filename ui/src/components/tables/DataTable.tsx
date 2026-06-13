import { useMemo } from 'react'
import {
  useReactTable,
  getCoreRowModel,
  createColumnHelper,
  flexRender,
  type SortingState,
} from '@tanstack/react-table'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import { Skeleton } from '@/components/ui/skeleton'
import { format } from 'date-fns'
import { ChevronLeft, ChevronRight, ChevronUp, ChevronDown, FileX, AlertCircle } from 'lucide-react'
import type { Field, Document } from '@/types/kora'

interface DataTableProps {
  columns: Field[]
  data: Document[]
  titleField: string
  total: number
  page: number
  totalPages: number
  sorting: { field: string; order: string } | null
  onSortingChange: (sorting: { field: string; order: string } | null) => void
  onPageChange: (page: number) => void
  onRowClick: (doc: Document) => void
  isEmpty: boolean
  isFetching: boolean
  isError: boolean
  onRetry: () => void
}

const columnHelper = createColumnHelper<Document>()

function renderCellValue(value: any, field: Field): React.ReactNode {
  if (value == null || value === '') return <span className="text-muted-foreground">—</span>

  switch (field.fieldtype) {
    case 'Check':
      return value ? '✓' : '✗'
    case 'Date':
      try { return format(new Date(value), 'MMM d, yyyy') } catch { return String(value) }
    case 'Datetime':
      try { return format(new Date(value), 'MMM d, yyyy HH:mm') } catch { return String(value) }
    case 'Select':
      return <Badge variant="secondary">{String(value)}</Badge>
    case 'Currency':
      return new Intl.NumberFormat('en-US', { style: 'currency', currency: 'USD' }).format(Number(value))
    case 'Percent':
      return `${Number(value)}%`
    case 'Int':
      return Number(value).toLocaleString()
    case 'Float':
      return Number(value).toFixed(2)
    case 'Link':
      return <span className="text-primary underline">{String(value)}</span>
    default:
      return String(value).length > 80 ? String(value).slice(0, 80) + '…' : String(value)
  }
}

export function DataTable({
  columns,
  data,
  titleField,
  total,
  page,
  totalPages,
  sorting,
  onSortingChange,
  onPageChange,
  onRowClick,
  isEmpty,
  isFetching,
  isError,
  onRetry,
}: DataTableProps) {
  const tableColumns = useMemo(
    () => [
      columnHelper.accessor('name', {
        header: 'Name',
        cell: (info) => <span className="font-mono text-xs">{info.getValue()}</span>,
      }),
      ...columns.map((field) =>
        columnHelper.accessor(field.fieldname, {
          header: field.label,
          cell: (info) => renderCellValue(info.getValue(), field),
        }),
      ),
    ],
    [columns],
  )

  const tableSorting: SortingState = sorting
    ? [{ id: sorting.field, desc: sorting.order === 'DESC' }]
    : []

  const table = useReactTable({
    data,
    columns: tableColumns,
    state: { sorting: tableSorting },
    getCoreRowModel: getCoreRowModel(),
    manualSorting: true,
    manualPagination: true,
  })

  const handleSort = (fieldname: string) => {
    if (fieldname === 'name') {
      if (sorting?.field === 'name') {
        onSortingChange(
          sorting.order === 'ASC' ? { field: 'name', order: 'DESC' } : null,
        )
      } else {
        onSortingChange({ field: 'name', order: 'ASC' })
      }
    } else {
      const field = columns.find((f) => f.fieldname === fieldname)
      if (!field) return
      if (sorting?.field === fieldname) {
        onSortingChange(
          sorting.order === 'ASC' ? { field: fieldname, order: 'DESC' } : null,
        )
      } else {
        onSortingChange({ field: fieldname, order: 'ASC' })
      }
    }
  }

  // Empty state.
  if (isEmpty && !isFetching) {
    return (
      <div className="flex flex-col items-center justify-center rounded-lg border border-dashed py-16 px-4">
        <FileX className="h-12 w-12 text-muted-foreground" />
        <h3 className="mt-4 text-lg font-medium text-center">No records found</h3>
        <p className="mt-1 text-sm text-muted-foreground text-center">Create your first document to get started.</p>
      </div>
    )
  }

  // Error state.
  if (isError && !isFetching) {
    return (
      <div className="flex flex-col items-center justify-center rounded-lg border border-dashed py-16 px-4">
        <AlertCircle className="h-12 w-12 text-destructive" />
        <h3 className="mt-4 text-lg font-medium text-center">Failed to load data</h3>
        <p className="mt-1 text-sm text-muted-foreground text-center">There was an error fetching the list.</p>
        <Button variant="outline" size="sm" className="mt-4" onClick={onRetry}>
          Retry
        </Button>
      </div>
    )
  }

  return (
    <div className="space-y-4">
      <div className="rounded-md border">
        {/* Desktop: standard table */}
        <div className="hidden md:block overflow-x-auto">
          <Table>
            <TableHeader>
              {table.getHeaderGroups().map((headerGroup) => (
                <TableRow key={headerGroup.id}>
                  {headerGroup.headers.map((header) => (
                    <TableHead
                      key={header.id}
                      className="cursor-pointer select-none"
                      onClick={() => handleSort(header.column.id)}
                    >
                      <div className="flex items-center gap-1">
                        {flexRender(header.column.columnDef.header, header.getContext())}
                        {sorting?.field === header.column.id ? (
                          sorting.order === 'ASC' ? (
                            <ChevronUp className="h-3 w-3" />
                          ) : (
                            <ChevronDown className="h-3 w-3" />
                          )
                        ) : null}
                      </div>
                    </TableHead>
                  ))}
                </TableRow>
              ))}
            </TableHeader>
            <TableBody>
              {isFetching && data.length === 0 ? (
                Array.from({ length: 5 }).map((_, i) => (
                  <TableRow key={i}>
                    {tableColumns.map((_, j) => (
                      <TableCell key={j}>
                        <Skeleton className="h-5 w-full" />
                      </TableCell>
                    ))}
                  </TableRow>
                ))
              ) : table.getRowModel().rows.length === 0 ? (
                <TableRow>
                  <TableCell colSpan={tableColumns.length} className="h-24 text-center">
                    No results.
                  </TableCell>
                </TableRow>
              ) : (
                table.getRowModel().rows.map((row) => (
                  <TableRow
                    key={row.id}
                    className="cursor-pointer hover:bg-muted/50"
                    onClick={() => onRowClick(row.original)}
                  >
                    {row.getVisibleCells().map((cell) => (
                      <TableCell key={cell.id}>
                        {flexRender(cell.column.columnDef.cell, cell.getContext())}
                      </TableCell>
                    ))}
                  </TableRow>
                ))
              )}
            </TableBody>
          </Table>
        </div>

        {/* Mobile: stacked card layout */}
        <div className="md:hidden divide-y">
          {isFetching && data.length === 0 ? (
            Array.from({ length: 3 }).map((_, i) => (
              <div key={i} className="p-4 space-y-3">
                {tableColumns.map((_, j) => (
                  <Skeleton key={j} className="h-4 w-full" />
                ))}
              </div>
            ))
          ) : table.getRowModel().rows.length === 0 ? (
            <div className="p-8 text-center text-muted-foreground text-sm">No results.</div>
          ) : (
            table.getRowModel().rows.map((row) => (
              <div
                key={row.id}
                className="p-4 cursor-pointer active:bg-muted/50 transition-colors"
                onClick={() => onRowClick(row.original)}
              >
                {row.getVisibleCells().map((cell, idx) => {
                  const header = cell.column.columnDef.header
                  const label = typeof header === 'string' ? header : ''
                  return (
                    <div key={cell.id} className="flex justify-between items-start py-1 gap-3">
                      <span className="text-xs font-medium text-muted-foreground shrink-0">{label}</span>
                      <span className={`text-sm text-right ${idx === 0 ? 'font-medium' : ''}`}>
                        {flexRender(cell.column.columnDef.cell, cell.getContext())}
                      </span>
                    </div>
                  )
                })}
              </div>
            ))
          )}
        </div>
      </div>

      {/* Pagination */}
      {totalPages > 1 && (
        <div className="flex items-center justify-between gap-2">
          <p className="text-sm text-muted-foreground hidden sm:block">
            Showing {page * (data.length > 0 ? 50 : 0) + 1}–{Math.min((page + 1) * 50, total)} of {total}
          </p>
          <div className="flex items-center gap-2">
            <Button
              variant="outline"
              size="sm"
              onClick={() => onPageChange(page - 1)}
              disabled={page === 0}
            >
              <ChevronLeft className="h-4 w-4" />
              <span className="hidden sm:inline ml-1">Previous</span>
            </Button>
            <span className="text-sm text-muted-foreground whitespace-nowrap">
              {page + 1}/{totalPages}
            </span>
            <Button
              variant="outline"
              size="sm"
              onClick={() => onPageChange(page + 1)}
              disabled={page >= totalPages - 1}
            >
              <span className="hidden sm:inline mr-1">Next</span>
              <ChevronRight className="h-4 w-4" />
            </Button>
          </div>
        </div>
      )}
    </div>
  )
}
