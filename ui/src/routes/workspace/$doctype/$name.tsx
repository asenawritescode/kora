import { useParams, useNavigate } from '@tanstack/react-router'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { fetchDoctypeSchema } from '@/lib/api/system'
import { fetchDocument, updateDocument } from '@/lib/api/resources'
import { applyComputedFields } from '@/lib/computed-fields'
import { FieldRenderer } from '@/components/forms/FieldRenderer'
import { RelatedDocs } from '@/components/forms/RelatedDocs'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import { Skeleton } from '@/components/ui/skeleton'
import { AlertCircle, ArrowLeft, Save, Loader2 } from 'lucide-react'
import { useState, useEffect, useMemo } from 'react'
import type { Field, DocType } from '@/types/kora'
import { Breadcrumbs } from '@/components/layout/Breadcrumbs'
import { ProgressBar } from '@/components/forms/ProgressBar'
import { FormSection, isLayoutField } from '@/components/forms/FormSection'
import { buildFormSections, isFieldRequired } from '@/components/forms/form-runtime'
import { toast } from '@/components/ui/Toast'
import { clearDocumentDraft, DraftPersistenceError, loadDocumentDraft, saveDocumentDraft } from '@/lib/draft-storage'
import { cn } from '@/lib/utils'
import { titleCase } from '../admin/doctypes/editor-helpers'
import { describeRealtimeState, useRealtimeConnection } from '@/lib/realtime'

export default function EditFormPage() {
  const { doctype, name } = useParams({ from: '/workspace/$doctype/$name' })
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({})
  const [formData, setFormData] = useState<Record<string, any>>({})
  const [draftLoaded, setDraftLoaded] = useState(false)
  const [draftStorageError, setDraftStorageError] = useState<string | null>(null)
  const realtime = useRealtimeConnection()

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

  const docQuery = useQuery({
    queryKey: ['resource', doctype, name],
    queryFn: () => fetchDocument(doctype, name),
  })

  const dt: DocType | undefined = schemaQuery.data?.doctype
  const displayName = dt ? titleCase(dt.name.replace(/_/g, ' ')) : ''
  const perms = schemaQuery.data?.permissions
  const canWrite = perms?.write ?? false
  const fields = useMemo(
    () => dt?.fields?.filter((f: Field) => !isLayoutField(f.fieldtype)) ?? [],
    [dt],
  )
  const layoutFields = dt?.fields ?? []
  const sections = useMemo(() => buildFormSections(layoutFields), [layoutFields])
  const hasSections = sections.length > 1
  useEffect(() => {
    if (!docQuery.data || fields.length === 0 || draftLoaded) return
    let cancelled = false

    void (async () => {
      const draft = await loadDocumentDraft<Record<string, any>>({ doctype, name })
      if (cancelled) return
      const base = docQuery.data as Record<string, any>
      const merged = draft?.value ? { ...base, ...draft.value } : base
      setFormData(applyComputedFields(fields, merged))
      setDraftLoaded(true)
    })()

    return () => {
      cancelled = true
    }
  }, [docQuery.data, fields, draftLoaded, doctype, name])

  useEffect(() => {
    if (!draftLoaded) return
    const timer = window.setTimeout(() => {
      if (Object.keys(formData).length === 0) return
      void saveDocumentDraft({ doctype, name }, formData).then(() => {
        setDraftStorageError(null)
      }).catch((cause: unknown) => {
        setDraftStorageError(cause instanceof DraftPersistenceError ? cause.message : 'The draft could not be saved in this browser.')
      })
    }, 350)
    return () => window.clearTimeout(timer)
  }, [doctype, formData, draftLoaded, name])

  const handleRowsChange = (fieldname: string, rows: Record<string, any>[]) => {
    setFormData((prev) => applyComputedFields(fields, { ...prev, [fieldname]: rows }))
  }

  const scrollToSection = (id: string) => {
    document.getElementById(id)?.scrollIntoView({ behavior: 'smooth', block: 'start' })
  }

  const handleFieldChange = (fieldname: string, value: any) => {
    setFormData((prev) => {
      const updated = applyComputedFields(fields, { ...prev, [fieldname]: value })
      const changedField = fields.find((f: Field) => f.fieldname === fieldname)
      if (changedField && (changedField.fieldtype === 'Link' || changedField.fieldtype === 'Dynamic Link') && value) {
        const targetDoctype = changedField.options
        const linkedFields = fields.filter((f: Field) => f.linked_field?.startsWith(fieldname + '.'))
        if (linkedFields.length > 0 && targetDoctype) {
          import('@/lib/api/resources').then(({ fetchDocument: fetchLinkedDocument }) => {
            fetchLinkedDocument(targetDoctype, value).then((doc) => {
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

  const handleSave = async () => {
    setSaving(true)
    setError(null)
    setFieldErrors({})
    try {
      await updateDocument(doctype, name, formData)
      queryClient.invalidateQueries({ queryKey: ['resource', doctype, name] })
      queryClient.invalidateQueries({ queryKey: ['resource', doctype] })
      queryClient.invalidateQueries({ queryKey: ['manifest-resource'] })
      let cleanupError: string | null = null
      try {
        await clearDocumentDraft({ doctype, name })
      } catch (cause) {
        cleanupError = cause instanceof DraftPersistenceError ? cause.message : 'The saved browser draft could not be cleared.'
      }
      toast('success', `${dt?.name || doctype} ${name} saved`)
      if (cleanupError) toast('error', cleanupError, 6000)
      navigate({ to: '/workspace/$doctype', params: { doctype } })
    } catch (err: any) {
      const msg = err.message || 'Failed to save'
      setError(msg)
      if (err.field) {
        setFieldErrors({ [err.field]: msg })
      }
      toast('error', msg, 6000)
    } finally {
      setSaving(false)
    }
  }

  if (schemaQuery.isLoading || docQuery.isLoading) {
    return (
      <div className="space-y-4 p-8">
        <Skeleton className="h-8 w-48" />
        <Skeleton className="h-96 w-full" />
      </div>
    )
  }

  if (!dt || !docQuery.data) {
    return (
      <div className="flex h-64 items-center justify-center">
        <p className="text-muted-foreground">Document not found.</p>
      </div>
    )
  }

  const statusField = dt.fields.find(
    (f: Field) => f.fieldname === 'status',
  )
  const statusValue = statusField ? formData[statusField.fieldname] : null
  const statusLabel = statusValue || `Draft (${docQuery.data.doc_status})`

  const requiredFields = fields.filter((f: Field) => isFieldRequired(f, formData))
  const filledRequired = requiredFields.filter((f: Field) => {
    const value = formData[f.fieldname]
    return value !== null && value !== undefined && value !== ''
  }).length

  const sectionCompletion = (sectionFields: Field[]) =>
    sectionFields.filter((field) => isFieldRequired(field, formData) && formData[field.fieldname] !== null && formData[field.fieldname] !== undefined && formData[field.fieldname] !== '').length

  return (
    <div className="p-4 md:p-8">
      <Breadcrumbs
        items={[
          { label: dt.module },
          { label: displayName, to: `/workspace/${encodeURIComponent(doctype)}` },
          { label: name },
        ]}
        className="mb-4"
      />

      {draftStorageError && (
        <div role="alert" className="mb-4 flex items-start gap-3 rounded-lg border border-destructive/50 bg-destructive/10 px-4 py-3 text-sm">
          <AlertCircle className="mt-0.5 h-4 w-4 shrink-0 text-destructive" />
          <p className="text-destructive">{draftStorageError}</p>
        </div>
      )}

      <div className="mb-4 flex flex-wrap items-center gap-3">
        <Button
          variant="ghost"
          size="icon"
          onClick={() => navigate({ to: '/workspace/$doctype', params: { doctype } })}
        >
          <ArrowLeft className="h-5 w-5" />
        </Button>
        <div className="min-w-0 flex-1">
          <h1 className="truncate text-xl font-bold md:text-2xl">
            {displayName}: {name}
          </h1>
          <div className="mt-1 flex flex-wrap items-center gap-2">
            <Badge variant="outline">{String(statusLabel)}</Badge>
            <span className="truncate font-mono text-xs text-muted-foreground">{name}</span>
          </div>
        </div>
        <Button onClick={handleSave} disabled={saving || !canWrite} size="lg">
          {saving ? <Loader2 className="mr-2 h-4 w-4 animate-spin" /> : <Save className="mr-2 h-4 w-4" />}
          {saving ? 'Saving...' : 'Save'}
        </Button>
      </div>

      <div className="mb-4 flex items-center gap-2 rounded-lg border bg-muted/20 px-3 py-2 text-xs">
        <Badge variant={describeRealtimeState(realtime).tone}>
          {describeRealtimeState(realtime).label}
        </Badge>
        <span className="text-muted-foreground">{describeRealtimeState(realtime).detail}</span>
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
            <p className="text-sm text-muted-foreground">This document has no editable fields.</p>
          ) : hasSections ? (
            sections.map((section, index) => (
              <FormSection
                key={`${section.title}-${index}`}
                id={`section-${index}`}
                title={section.title}
                defaultOpen={index < 3}
                badge={`${sectionCompletion(section.fields)}/${section.fields.filter((field) => isFieldRequired(field, formData)).length}`}
              >
                {section.fields.map((field: Field) => (
                  <FieldRenderer
                    key={field.fieldname}
                    field={field}
                    value={formData[field.fieldname] ?? null}
                    onChange={handleFieldChange}
                    onRowsChange={handleRowsChange}
                    disabled={saving || !canWrite}
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
                disabled={saving || !canWrite}
                error={fieldErrors[field.fieldname]}
                formData={formData}
              />
            ))
          )}

          <RelatedDocs doctype={doctype} name={name} />
        </div>

        <aside className="space-y-4 xl:sticky xl:top-20 h-fit">
          <div className="rounded-xl border bg-card p-4 shadow-sm">
            <ProgressBar filled={filledRequired} total={requiredFields.length} />
            <p className="mt-3 text-xs text-muted-foreground">
              Save, required-field progress, and section chips keep the workflow simple on mobile.
            </p>
          </div>

          <div className="rounded-xl border bg-card p-4 shadow-sm">
            <p className="text-xs font-medium uppercase tracking-wide text-muted-foreground">Actions</p>
            <ul className="mt-3 space-y-2 text-sm text-muted-foreground">
              <li>Use the section chips to jump around long records.</li>
              <li>Drafts are auto-saved locally while editing.</li>
              <li>Related docs appear inline at the bottom.</li>
            </ul>
          </div>
        </aside>
      </div>
    </div>
  )
}
