import { useParams, useNavigate } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { fetchDoctypeSchema } from '@/lib/api/system'
import { createDocument } from '@/lib/api/resources'
import { FieldRenderer } from '@/components/forms/FieldRenderer'
import { Button } from '@/components/ui/button'
import { Skeleton } from '@/components/ui/skeleton'
import { AlertCircle, ArrowLeft, Loader2 } from 'lucide-react'
import { useState } from 'react'
import { applyComputedFields } from '@/lib/computed-fields'
import type { Field, DocType } from '@/types/kora'

export default function NewFormPage() {
  const { doctype } = useParams({ from: '/workspace/$doctype/new' })
  const navigate = useNavigate()
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({})
  const [formData, setFormData] = useState<Record<string, any>>({})

  const schemaQuery = useQuery({
    queryKey: ['doctype', doctype],
    queryFn: () => fetchDoctypeSchema(doctype),
    staleTime: 5 * 60_000,
  })

  const dt: DocType | undefined = schemaQuery.data?.doctype
  const fields = dt?.fields?.filter((f: Field) => !isLayoutField(f.fieldtype)) ?? []

  const handleRowsChange = (rows: Record<string, any>[]) => {
    setFormData((prev) => applyComputedFields(fields, { ...prev, items: rows }))
  }

  const handleFieldChange = (fieldname: string, value: any) => {
    setFormData((prev) => {
      const updated = applyComputedFields(fields, { ...prev, [fieldname]: value })
      // Auto-populate linked_field references when a Link field changes.
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
      await createDocument(doctype, formData)
      setFormData({})
      navigate({ to: '/workspace/$doctype', params: { doctype } })
    } catch (err: any) {
      const msg = err.message || 'Failed to create'
      setError(msg) // Always show banner.
      if (err.field) {
        setFieldErrors({ [err.field]: msg }) // Also show inline.
      }
    } finally {
      setSaving(false)
    }
  }

  if (schemaQuery.isLoading) {
    return (
      <div className="p-8 space-y-4">
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
    <div className="p-8 max-w-3xl">
      {/* Header */}
      <div className="mb-6 flex items-center gap-4">
        <Button
          variant="ghost"
          size="icon"
          onClick={() => navigate({ to: '/workspace/$doctype', params: { doctype } })}
        >
          <ArrowLeft className="h-5 w-5" />
        </Button>
        <div>
          <h1 className="text-2xl font-bold">New {dt.name}</h1>
          <p className="text-sm text-muted-foreground">Create a new {dt.name.toLowerCase()} document</p>
        </div>
      </div>

      {error && (
        <div className="mb-4 flex items-start gap-3 rounded-lg border border-destructive/50 bg-destructive/10 px-4 py-3 text-sm"><AlertCircle className="mt-0.5 h-4 w-4 shrink-0 text-destructive" /><p className="text-destructive">{error}</p></div>
      )}

      {/* Form fields */}
      <div className="space-y-6 rounded-lg border p-6">
        {fields.length === 0 ? (
          <p className="text-sm text-muted-foreground">
            This DocType has no data fields. The document will be created with default values.
          </p>
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
            />
          ))
        )}

        <div className="flex gap-3 pt-4">
          <Button onClick={handleSubmit} disabled={saving} size="lg">
            {saving && <Loader2 className="mr-2 h-4 w-4 animate-spin" />}
            Create {dt.name}
          </Button>
          <Button
            variant="outline"
            onClick={() => navigate({ to: '/workspace/$doctype', params: { doctype } })}
          >
            Cancel
          </Button>
        </div>
      </div>
    </div>
  )
}

function isLayoutField(fieldtype: string): boolean {
  return ['Section Break', 'Column Break', 'Heading'].includes(fieldtype)
}
