import { useParams, useNavigate } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { fetchDoctypeSchema } from '@/lib/api/system'
import { createDocument } from '@/lib/api/resources'
import { FieldRenderer } from '@/components/forms/FieldRenderer'
import { Button } from '@/components/ui/button'
import { Skeleton } from '@/components/ui/skeleton'
import { AlertCircle, ArrowLeft, Loader2 } from 'lucide-react'
import { applyComputedFields } from '@/lib/computed-fields'
import type { Field, DocType } from '@/types/kora'
import { Breadcrumbs } from '@/components/layout/Breadcrumbs'
import { FormSection, isLayoutField } from '@/components/forms/FormSection'
import { buildDefaultFormData, buildFormSections } from '@/components/forms/form-runtime'
import { toast } from '@/components/ui/Toast'
import { clearDocumentDraft, DraftPersistenceError, loadDocumentDraft, saveDocumentDraft } from '@/lib/draft-storage'
import { Badge } from '@/components/ui/badge'
import { cn } from '@/lib/utils'
import { useEffect, useMemo, useRef, useState } from 'react'
import { titleCase } from '../admin/doctypes/editor-helpers'

export default function NewFormPage() {
  const { doctype } = useParams({ from: '/workspace/$doctype/new' })
  const navigate = useNavigate()
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({})
  const [formData, setFormData] = useState<Record<string, any>>({})
  const [draftSavedAt, setDraftSavedAt] = useState<Date | null>(null)
  const [draftRestored, setDraftRestored] = useState(false)
  const [draftStorageError, setDraftStorageError] = useState<string | null>(null)

  if (doctype === 'pages') {
    return (
      <div className="flex h-64 items-center justify-center">
        <p className="text-muted-foreground">The /workspace/pages route has been removed.</p>
      </div>
    )
  }

  const schemaQuery = useQuery({
    queryKey: ['doctype', doctype],
    queryFn: () => fetchDoctypeSchema(doctype),
    staleTime: 5 * 60_000,
  })

  const dt: DocType | undefined = schemaQuery.data?.doctype
  const displayName = dt ? titleCase(dt.name.replace(/_/g, ' ')) : ''
  const fields = dt?.fields?.filter((f: Field) => !isLayoutField(f.fieldtype)) ?? []
  const layoutFields = dt?.fields ?? []
  const draftLoadedRef = useRef(false)
  const firstFieldFocusedRef = useRef(false)
  const sections = useMemo(() => buildFormSections(layoutFields), [layoutFields])
  const hasSections = sections.length > 1
  const requiredFields = useMemo(() => fields.filter((f: Field) => f.reqd), [fields])
  const filledRequired = useMemo(
    () =>
      requiredFields.filter((f: Field) => {
        const value = formData[f.fieldname]
        return value !== null && value !== undefined && value !== ''
      }).length,
    [formData, requiredFields],
  )

  const handleRowsChange = (fieldname: string, rows: Record<string, any>[]) => {
    setFormData((prev) => applyComputedFields(fields, { ...prev, [fieldname]: rows }))
  }

  const scrollToSection = (id: string) => {
    document.getElementById(id)?.scrollIntoView({ behavior: 'smooth', block: 'start' })
  }

  useEffect(() => {
    if (!dt || draftLoadedRef.current) return
    let cancelled = false

    void (async () => {
      const draft = await loadDocumentDraft<Record<string, any>>({ doctype })
      if (cancelled) return
      if (draft?.value) {
        setFormData(applyComputedFields(fields, { ...buildDefaultFormData(fields), ...draft.value }))
        setDraftRestored(true)
      } else {
        setFormData(applyComputedFields(fields, buildDefaultFormData(fields)))
      }
      draftLoadedRef.current = true
    })()

    return () => {
      cancelled = true
    }
  }, [dt, doctype, fields])

  useEffect(() => {
    if (!draftLoadedRef.current || firstFieldFocusedRef.current || Object.keys(formData).length === 0) return
    const firstField = document.querySelector<HTMLElement>(
      '#section-0 input:not([type="hidden"]), #section-0 textarea, #section-0 [role="combobox"]',
    )
    if (firstField) {
      firstField.focus()
      firstFieldFocusedRef.current = true
    }
  }, [formData, dt])

  useEffect(() => {
    if (!draftLoadedRef.current) return
    const timer = window.setTimeout(() => {
      if (Object.keys(formData).length === 0) return
      void saveDocumentDraft({ doctype }, formData).then(() => {
        setDraftSavedAt(new Date())
        setDraftStorageError(null)
      }).catch((cause: unknown) => {
        setDraftSavedAt(null)
        setDraftStorageError(cause instanceof DraftPersistenceError ? cause.message : 'The draft could not be saved in this browser.')
      })
    }, 350)
    return () => window.clearTimeout(timer)
  }, [doctype, formData])

  const handleFieldChange = (fieldname: string, value: any) => {
    setFormData((prev) => {
      const updated = applyComputedFields(fields, { ...prev, [fieldname]: value })
      const changedField = fields.find((f: Field) => f.fieldname === fieldname)
      if (changedField && (changedField.fieldtype === 'Link' || changedField.fieldtype === 'Dynamic Link') && value) {
        const targetDoctype = changedField.options
        const linkedFields = fields.filter((f: Field) => f.linked_field?.startsWith(fieldname + '.'))
        if (linkedFields.length > 0 && targetDoctype) {
          import('@/lib/api/resources').then(({ fetchDocument }) => {
            fetchDocument(targetDoctype, value).then((doc) => {
              setFormData((prev2) => {
                const withLinked = { ...prev2 }
                for (const lf of linkedFields) {
                  const sourceField = lf.linked_field!.split('.')[1]
                  if (doc[sourceField] !== undefined) {
                    withLinked[lf.fieldname] = doc[sourceField]
                  }
                }
                return applyComputedFields(fields, withLinked)
              })
            }).catch(() => {})
          })
        }
      }
      return updated
    })
  }

  const handleSubmit = async () => {
    setSaving(true)
    setError(null)
    setFieldErrors({})
    try {
      const created = await createDocument(doctype, formData)
      setFormData({})
      let cleanupError: string | null = null
      try {
        await clearDocumentDraft({ doctype })
      } catch (cause) {
        cleanupError = cause instanceof DraftPersistenceError ? cause.message : 'The saved browser draft could not be cleared.'
      }
      toast('success', `${dt?.name || doctype} created`)
      if (cleanupError) toast('error', cleanupError, 6000)
      navigate({ to: '/workspace/$doctype/$name', params: { doctype, name: created.name } })
    } catch (err: any) {
      const msg = err.message || 'Failed to create'
      setError(msg)
      if (err.field) {
        setFieldErrors({ [err.field]: msg })
      }
      toast('error', msg, 6000)
    } finally {
      setSaving(false)
    }
  }

  useEffect(() => {
    const handleShortcut = (event: KeyboardEvent) => {
      if ((event.ctrlKey || event.metaKey) && event.key.toLowerCase() === 's') {
        event.preventDefault()
        if (!saving) void handleSubmit()
      }
    }
    window.addEventListener('keydown', handleShortcut)
    return () => window.removeEventListener('keydown', handleShortcut)
  }, [saving, formData])

  if (schemaQuery.isLoading) {
    return (
      <div className="space-y-4 p-8">
        <Skeleton className="h-8 w-48" />
        <Skeleton className="h-96 w-full" />
      </div>
    )
  }

  if (!dt) {
    return (
      <div className="flex h-64 items-center justify-center">
        <p className="text-muted-foreground">DocType "{doctype}" not found.</p>
      </div>
    )
  }

  return (
    <div className="p-4 md:p-8">
      <Breadcrumbs
        items={[
          { label: dt.module },
          { label: displayName, to: `/workspace/${encodeURIComponent(doctype)}` },
          { label: `New ${displayName}` },
        ]}
        className="mb-4"
      />

      {draftStorageError && (
        <div role="alert" className="mb-4 flex items-start gap-3 rounded-lg border border-destructive/50 bg-destructive/10 px-4 py-3 text-sm">
          <AlertCircle className="mt-0.5 h-4 w-4 shrink-0 text-destructive" />
          <p className="text-destructive">{draftStorageError}</p>
        </div>
      )}

      <div className="mb-6 flex flex-wrap items-center gap-4">
        <Button
          variant="ghost"
          size="icon"
          onClick={() => navigate({ to: '/workspace/$doctype', params: { doctype } })}
        >
          <ArrowLeft className="h-5 w-5" />
        </Button>
        <div className="min-w-0 flex-1">
          <h1 className="truncate text-2xl font-bold">New {displayName}</h1>
          <p className="text-sm text-muted-foreground">Create a new {displayName.toLowerCase()} document</p>
        </div>
        <Badge variant="outline">{filledRequired}/{requiredFields.length || 0} required</Badge>
      </div>

      {error && (
        <div className="mb-4 flex items-start gap-3 rounded-lg border border-destructive/50 bg-destructive/10 px-4 py-3 text-sm">
          <AlertCircle className="mt-0.5 h-4 w-4 shrink-0 text-destructive" />
          <p className="text-destructive">{error}</p>
        </div>
      )}

      <div className="grid gap-6 xl:grid-cols-[minmax(0,1fr)_280px]">
        <div className="space-y-4 rounded-xl border bg-card p-4 shadow-sm md:p-6">
          {hasSections && (
            <div className="sticky top-14 z-10 -mx-4 border-b bg-card/95 px-4 pb-4 backdrop-blur md:-mx-6 md:px-6">
              <div className="flex items-center justify-between gap-3">
                <div>
                  <p className="text-xs font-medium uppercase tracking-wide text-muted-foreground">Sections</p>
                  <p className="text-sm text-muted-foreground">{sections.length} groups</p>
                </div>
                <div className="text-right">
                  <p className="text-xs text-muted-foreground">Progress</p>
                  <p className="text-sm font-medium">{filledRequired}/{requiredFields.length || 0}</p>
                </div>
              </div>
              <div className="mt-4 flex gap-2 overflow-x-auto pb-1">
                {sections.map((section, index) => (
                  <button
                    key={`${section.title}-${index}`}
                    type="button"
                    onClick={() => scrollToSection(`section-${index}`)}
                    className={cn(
                      'whitespace-nowrap rounded-full border px-3 py-1 text-xs font-medium transition-colors',
                      'bg-background hover:bg-muted',
                    )}
                  >
                    {section.title}
                  </button>
                ))}
              </div>
            </div>
          )}

          {fields.length === 0 ? (
            <p className="text-sm text-muted-foreground">
              This DocType has no data fields. The document will be created with default values.
            </p>
          ) : hasSections ? (
            sections.map((section, index) => (
              <FormSection
                key={`${section.title}-${index}`}
                id={`section-${index}`}
                title={section.title}
                defaultOpen={index < 3}
                badge={`${section.fields.filter((field) => field.reqd && formData[field.fieldname] !== null && formData[field.fieldname] !== undefined && formData[field.fieldname] !== '').length}/${section.fields.filter((field) => field.reqd).length}`}
              >
                {section.fields.map((field: Field) => (
                  <FieldRenderer
                    key={field.fieldname}
                    field={field}
                    value={formData[field.fieldname] ?? null}
                    onChange={handleFieldChange}
                    onRowsChange={handleRowsChange}
                    disabled={saving}
                    error={fieldErrors[field.fieldname]}
                    formData={formData}
                  />
                ))}
              </FormSection>
            ))
          ) : (
            fields.map((field: Field) => (
              <FieldRenderer
                key={field.fieldname}
                field={field}
                value={formData[field.fieldname] ?? null}
                onChange={handleFieldChange}
                onRowsChange={handleRowsChange}
                disabled={saving}
                error={fieldErrors[field.fieldname]}
                formData={formData}
              />
            ))
          )}

          <div className="flex flex-col gap-3 border-t pt-4 sm:flex-row sm:justify-end">
            <Button
              variant="outline"
              onClick={() => navigate({ to: '/workspace/$doctype', params: { doctype } })}
            >
              Cancel
            </Button>
            <Button onClick={handleSubmit} disabled={saving} size="lg">
              {saving && <Loader2 className="mr-2 h-4 w-4 animate-spin" />}
              Create {displayName}
            </Button>
          </div>
          <p className="text-right text-xs text-muted-foreground">
            Tip: press Ctrl/⌘ + S to create this document.
          </p>
        </div>

        <aside className="space-y-4 xl:sticky xl:top-20 h-fit">
          <div className="rounded-xl border bg-card p-4 shadow-sm">
            <p className="text-xs font-medium uppercase tracking-wide text-muted-foreground">Completion</p>
            <p className="mt-1 text-sm font-medium">
              {filledRequired}/{requiredFields.length || 0} required fields filled
            </p>
            <div className="mt-3 h-2 rounded-full bg-muted">
              <div
                className="h-2 rounded-full bg-primary transition-all"
                style={{ width: `${requiredFields.length ? Math.round((filledRequired / requiredFields.length) * 100) : 0}%` }}
              />
            </div>
            <p className="mt-3 text-xs text-muted-foreground">
              Section chips let you jump through the form quickly on desktop and mobile.
            </p>
          </div>

          <div className="rounded-xl border bg-card p-4 shadow-sm">
            <p className="text-xs font-medium uppercase tracking-wide text-muted-foreground">Drafts</p>
            {draftRestored && (
              <p className="mt-2 text-xs text-primary">Draft restored from local storage.</p>
            )}
            {draftSavedAt ? (
              <p className="mt-2 text-xs text-muted-foreground">
                Draft saved at {draftSavedAt.toLocaleTimeString()}
              </p>
            ) : (
              <p className="mt-2 text-xs text-muted-foreground">
                Drafts are saved automatically as you type.
              </p>
            )}
          </div>
        </aside>
      </div>
    </div>
  )
}
