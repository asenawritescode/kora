import { useParams, useNavigate } from '@tanstack/react-router'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { fetchDoctypeSchema } from '@/lib/api/system'
import { fetchDocument, updateDocument } from '@/lib/api/resources'
import { applyComputedFields } from '@/lib/computed-fields'
import { FieldRenderer } from '@/components/forms/FieldRenderer'
import { RelatedDocs } from '@/components/forms/RelatedDocs'
import { WorkflowActions } from '@/components/forms/WorkflowActions'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import { Skeleton } from '@/components/ui/skeleton'
import { AlertCircle, ArrowLeft, Save, Loader2 } from 'lucide-react'
import { useState, useEffect, useMemo } from 'react'
import type { Field, DocType } from '@/types/kora'
import { Breadcrumbs } from '@/components/layout/Breadcrumbs'
import { ProgressBar } from '@/components/forms/ProgressBar'
import { FormSection, isLayoutField } from '@/components/forms/FormSection'
import { toast } from '@/components/ui/Toast'

export default function EditFormPage() {
  const { doctype, name } = useParams({ from: '/workspace/$doctype/$name' })
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({})
  const [formData, setFormData] = useState<Record<string, any>>({})

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
  const perms = schemaQuery.data?.permissions
  const canWrite = perms?.write ?? false
  const fields = dt?.fields?.filter(
    (f: Field) => !isLayoutField(f.fieldtype),
  ) ?? []

  // Populate form data when document loads, applying computed fields.
  useEffect(() => {
    if (docQuery.data && fields.length > 0) {
      setFormData(applyComputedFields(fields, docQuery.data as Record<string, any>))
    }
  }, [docQuery.data, schemaQuery.data])

  const handleRowsChange = (rows: Record<string, any>[]) => {
    setFormData((prev) => applyComputedFields(fields, { ...prev, items: rows }))
  }

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

  const handleSave = async () => {
    setSaving(true)
    setError(null)
    setFieldErrors({})
    try {
      await updateDocument(doctype, name, formData)
      queryClient.invalidateQueries({ queryKey: ['resource', doctype, name] })
      queryClient.invalidateQueries({ queryKey: ['resource', doctype] })
      navigate({ to: '/workspace/$doctype', params: { doctype } })
    } catch (err: any) {
      const msg = err.message || 'Failed to save'
      setError(msg) // Always show banner.
      if (err.field) {
        setFieldErrors({ [err.field]: msg }) // Also show inline.
      }
    } finally {
      setSaving(false)
      toast("success", `${dt?.name || doctype} ${name} saved`)
    }
  }

  if (schemaQuery.isLoading || docQuery.isLoading) {
    return (
      <div className="p-8 space-y-4">
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
    (f: Field) => f.fieldname === (schemaQuery.data?.workflow?.state_field || 'status'),
  )
  const statusValue = statusField ? formData[statusField.fieldname] : null
  const statusLabel = statusValue || `Draft (${docQuery.data.doc_status})`

  // Compute required field count for progress bar.
  const requiredFields = fields.filter((f: Field) => f.reqd)
  const filledRequired = requiredFields.filter((f: Field) => {
    const v = formData[f.fieldname]
    return v !== null && v !== undefined && v !== ''
  }).length

  // Group fields into sections by Section Break.
  const sections: { title: string; fields: Field[] }[] = []
  let currentSection: Field[] = []
  for (const f of fields) {
    if (f.fieldtype === 'Section Break') {
      if (currentSection.length > 0) sections.push({ title: f.label || 'Section', fields: currentSection })
      currentSection = []
    } else if (f.fieldtype === 'Heading') {
      if (currentSection.length > 0) sections.push({ title: f.label || '', fields: currentSection })
      currentSection = []
    } else if (!isLayoutField(f.fieldtype)) {
      currentSection.push(f)
    }
  }
  if (currentSection.length > 0) sections.push({ title: 'Details', fields: currentSection })
  const hasSections = sections.length > 1

  return (
    <div className="p-4 md:p-8 max-w-3xl">
      {/* Breadcrumbs */}
      <Breadcrumbs items={[{ label: dt.name, to: `/workspace/${encodeURIComponent(doctype)}` }, { label: name }]} className="mb-4" />

      {/* Header */}
      <div className="mb-4 flex items-center gap-3 flex-wrap">
        <Button
          variant="ghost"
          size="icon"
          onClick={() => navigate({ to: '/workspace/$doctype', params: { doctype } })}
        >
          <ArrowLeft className="h-5 w-5" />
        </Button>
        <div className="flex-1 min-w-0">
          <h1 className="text-xl md:text-2xl font-bold truncate">
            {dt.name}: {name}
          </h1>
          <div className="mt-1 flex items-center gap-2 flex-wrap">
            <Badge variant="outline">{String(statusLabel)}</Badge>
            <span className="text-xs text-muted-foreground font-mono truncate">{name}</span>
          </div>
        </div>
        <Button onClick={handleSave} disabled={saving || !canWrite} size="lg">
          {saving ? <Loader2 className="mr-2 h-4 w-4 animate-spin" /> : <Save className="mr-2 h-4 w-4" />}
          {saving ? 'Saving...' : 'Save'}
        </Button>
      </div>

      {/* Progress bar */}
      {requiredFields.length > 0 && (
        <ProgressBar filled={filledRequired} total={requiredFields.length} className="mb-4" />
      )}

      {error && (
        <div className="mb-4 flex items-start gap-3 rounded-lg border border-destructive/50 bg-destructive/10 px-4 py-3 text-sm"><AlertCircle className="mt-0.5 h-4 w-4 shrink-0 text-destructive" /><p className="text-destructive">{error}</p></div>
      )}

      {/* Workflow actions */}
      {schemaQuery.data?.workflow && (
        <WorkflowActions
          doctype={doctype}
          name={name}
          currentState={String(statusLabel)}
        />
      )}

      {/* Form fields — grouped into collapsible sections */}
      <div className="space-y-4">
        {fields.length === 0 ? (
          <p className="text-sm text-muted-foreground">This document has no editable fields.</p>
        ) : hasSections ? (
          sections.map((section, i) => (
            <FormSection
              key={i}
              title={section.title}
              defaultOpen={i < 3}
              badge={`${section.fields.filter((f: Field) => f.reqd && formData[f.fieldname] !== null && formData[f.fieldname] !== undefined && formData[f.fieldname] !== '').length}/${section.fields.filter((f: Field) => f.reqd).length} req'd`}
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
            />
          ))
        )}

        {/* Related documents (back-references) */}
        <RelatedDocs doctype={doctype} name={name} />
      </div>
    </div>
  )
}

