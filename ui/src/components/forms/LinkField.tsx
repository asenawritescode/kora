import { useState, useEffect, useRef, useCallback } from 'react'
import { useQuery } from '@tanstack/react-query'
import { fetchList } from '@/lib/api/resources'
import { fetchDoctypeSchema } from '@/lib/api/system'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Loader2, Check, ChevronsUpDown, X } from 'lucide-react'
import { cn } from '@/lib/utils'
import type { Field, Document } from '@/types/kora'
import { linkPickerDisplayFields, linkPickerSearchParams } from './link-search'

interface LinkFieldProps {
  field: Field
  value: any
  onChange: (fieldname: string, value: any) => void
  disabled: boolean
  error?: string
  compact?: boolean
}

export function LinkField({ field, value, onChange, disabled, error, compact }: LinkFieldProps) {
  const targetDoctype = field.options
  const [open, setOpen] = useState(false)
  const [search, setSearch] = useState('')
  const [serverResults, setServerResults] = useState<Document[]>([])
  const [isSearching, setIsSearching] = useState(false)
  const searchTimeout = useRef<ReturnType<typeof setTimeout> | undefined>(undefined)
  const searchRequestID = useRef(0)
  const containerRef = useRef<HTMLDivElement | null>(null)

  // Fetch target doctype schema to get title_field.
  const schemaQuery = useQuery({
    queryKey: ['doctype', targetDoctype],
    queryFn: () => fetchDoctypeSchema(targetDoctype!),
    enabled: !!targetDoctype,
    staleTime: 5 * 60_000,
  })

  const titleField = schemaQuery.data?.doctype?.title_field || 'name'
  const displayFields = linkPickerDisplayFields(schemaQuery.data?.doctype)

  // Load first 50 records for initial display.
  const listQuery = useQuery({
    queryKey: ['resource', targetDoctype, 'linkfield', displayFields.join(',')],
    queryFn: () => fetchList(targetDoctype!, { limit: 50, fields: displayFields }),
    enabled: !!targetDoctype,
    staleTime: 30_000,
  })

  const candidates: Document[] = listQuery.data?.data ?? []

  const doServerSearch = useCallback(
    async (term: string, requestID: number) => {
      if (term.length < 2 || !targetDoctype) return
      try {
        const result = await fetchList(targetDoctype, linkPickerSearchParams(term, displayFields))
        if (searchRequestID.current === requestID) setServerResults(result.data ?? [])
      } catch {
        if (searchRequestID.current === requestID) setServerResults([])
      } finally {
        if (searchRequestID.current === requestID) setIsSearching(false)
      }
    },
    [targetDoctype, displayFields.join(',')],
  )

  useEffect(() => {
    if (searchTimeout.current) clearTimeout(searchTimeout.current)
    const requestID = ++searchRequestID.current
    setServerResults([])
    searchTimeout.current = setTimeout(() => {
      if (search.trim().length >= 2) {
        setIsSearching(true)
        doServerSearch(search.trim(), requestID)
      } else {
        setIsSearching(false)
      }
    }, 300)
    return () => { if (searchTimeout.current) clearTimeout(searchTimeout.current) }
  }, [search, candidates, titleField, doServerSearch])

  // Close on outside click.
  useEffect(() => {
    const handler = (e: MouseEvent) => {
      if (containerRef.current && !containerRef.current.contains(e.target as Node)) {
        setOpen(false)
      }
    }
    if (open) document.addEventListener('mousedown', handler)
    return () => document.removeEventListener('mousedown', handler)
  }, [open])

  // Deduplicate combined results.
  const allResults = [...candidates, ...serverResults].filter(
    (d, i, arr) => arr.findIndex((x) => x.name === d.name) === i,
  )

  // Typed search is executed by Engine against the target DocType's configured
  // indexed fields before pagination. Do not re-filter a partial client cache:
  // doing so hides valid database matches (and can show stale cached rows).
  const filtered = search.trim().length >= 2
    ? serverResults
    : search.length > 0
      ? []
      : allResults.slice(0, 20)

  const currentDoc = allResults.find((d) => d.name === value)
  const currentTitle = currentDoc ? String(currentDoc[titleField] ?? '') : ''
  const displayValue = currentDoc
    ? (currentTitle && currentTitle !== currentDoc.name ? currentTitle : currentDoc.name)
    : (value || '')

  if (!targetDoctype) {
    return (
      <div className="space-y-1.5">
        <Label>{field.label}</Label>
        <Input
          value={value ?? ''}
          onChange={(e) => onChange(field.fieldname, e.target.value)}
          disabled={disabled}
        />
      </div>
    )
  }

  return (
    <div className="space-y-1.5" ref={containerRef}>
      <Label className={field.bold ? 'font-semibold' : ''}>
        {field.label}{field.reqd && <span className="ml-1 text-destructive">*</span>}
      </Label>

      <div className="relative">
        <div
          className={cn(
            'flex items-center rounded-md border bg-background text-foreground',
            compact && 'h-8 text-sm',
            error && 'border-destructive',
            disabled && 'opacity-50',
          )}
        >
          <input
            type="text"
            className={cn(
              'flex-1 bg-transparent px-2 text-foreground outline-none placeholder:text-muted-foreground',
              compact ? 'py-1 text-xs' : 'px-3 py-2 text-sm',
            )}
            placeholder={compact ? field.label : `Search ${targetDoctype}...`}
            value={open ? search : displayValue}
            onFocus={() => { setOpen(true); setSearch('') }}
            onChange={(e) => {
              setSearch(e.target.value)
              setOpen(true)
            }}
            disabled={disabled}
          />
          {value && !open && (
            <button
              type="button"
              className={cn('text-muted-foreground hover:text-foreground', compact ? 'px-0.5' : 'px-2')}
              onClick={() => { onChange(field.fieldname, ''); setSearch('') }}
              tabIndex={-1}
            >
              <X className={compact ? 'h-3 w-3' : 'h-4 w-4'} />
            </button>
          )}
          <span className={cn('text-muted-foreground', compact ? 'px-0.5' : 'px-2')}>
            {isSearching ? (
              <Loader2 className={compact ? 'h-3 w-3 animate-spin' : 'h-4 w-4 animate-spin'} />
            ) : (
              <ChevronsUpDown className={compact ? 'h-3 w-3' : 'h-4 w-4'} />
            )}
          </span>
        </div>

        {/* Dropdown */}
        {open && !disabled && (
          <div className="absolute z-50 mt-1 w-full rounded-md border bg-popover shadow-md">
            <div className="max-h-60 overflow-auto p-1">
              {filtered.length === 0 ? (
                <p className="py-6 text-center text-sm text-muted-foreground">
                  {listQuery.isLoading ? 'Loading...' : isSearching ? 'Searching...' : search.trim().length >= 2 ? 'No results.' : 'Type to search...'}
                </p>
              ) : (
                filtered.map((doc) => (
                  <button
                    key={doc.name}
                    type="button"
                    className={cn(
                      'flex w-full items-center rounded-sm px-2 py-1.5 text-left text-sm hover:bg-accent',
                      value === doc.name && 'bg-accent',
                    )}
                    onClick={() => {
                      onChange(field.fieldname, doc.name)
                      setOpen(false)
                      setSearch('')
                    }}
                  >
                    <Check className={cn('mr-2 h-4 w-4 shrink-0', value === doc.name ? 'opacity-100' : 'opacity-0')} />
                    <div className="flex flex-col min-w-0">
                      <span className="truncate text-sm">{String(doc[titleField] || doc.name)}</span>
                      {doc.name !== doc[titleField] && <span className="text-xs text-muted-foreground truncate">{doc.name}</span>}
                    </div>
                  </button>
                ))
              )}
            </div>
          </div>
        )}
      </div>

      {error && <p className={cn('text-destructive', compact ? 'text-[10px]' : 'text-xs')}>{error}</p>}
    </div>
  )
}
