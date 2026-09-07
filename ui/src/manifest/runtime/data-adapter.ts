import type { ManifestResourceState } from './ManifestRenderer'

export interface GeneratedResourceDescriptor {
  id: string
  doctype: string
  label: string
  fields: Array<{ name: string; label: string; type: 'text' | 'date' | 'status' | 'email' }>
}

export interface GeneratedDataAdapter {
  describe(): GeneratedResourceDescriptor[]
  load(resourceId: string): Promise<ManifestResourceState>
}

type KoraField = { fieldname: string; fieldtype: string; label?: string; hidden?: boolean }
type KoraDocType = { name: string; fields?: KoraField[]; status?: string }

const fieldType = (type: string): GeneratedResourceDescriptor['fields'][number]['type'] => {
  if (/date|time/i.test(type)) return 'date'
  if (/email/i.test(type)) return 'email'
  if (/select|status|check/i.test(type)) return 'status'
  return 'text'
}

async function koraJson<T>(path: string): Promise<T> {
  const response = await fetch(path, { credentials: 'same-origin' })
  if (!response.ok) throw new Error(`Kora request failed (${response.status})`)
  const body = await response.json()
  return (body?.data ?? body) as T
}

export const koraStudioDataAdapter: GeneratedDataAdapter = {
  describe: () => [],
  load: async (resourceId) => {
    try {
      const doctypes = await koraJson<KoraDocType[]>('/api/v1/system/doctypes')
      const doctype = doctypes.find((item) => item.name === resourceId || item.name.toLowerCase() === resourceId.toLowerCase())
        ?? doctypes.find((item) => item.status !== 'Draft')
      if (!doctype) return { kind: 'empty', data: { data: [], meta: { total: 0 } } }
      const records = await koraJson<Record<string, unknown>[]>(
        `/api/v1/resource/${encodeURIComponent(doctype.name)}?limit=50`,
      )
      return { kind: 'normal', data: { data: records, meta: { total: records.length, doctype: doctype.name, source: 'Kora' } } }
    } catch (error) {
      return { kind: 'error', error: error instanceof Error ? error.message : 'Unable to load Kora data' }
    }
  },
}

export async function describeKoraResources(): Promise<GeneratedResourceDescriptor[]> {
  const doctypes = await koraJson<KoraDocType[]>('/api/v1/system/doctypes')
  return doctypes.filter((item) => item.status !== 'Draft').slice(0, 100).map((item) => ({
    id: item.name,
    doctype: item.name,
    label: item.name,
    fields: (item.fields ?? []).filter((field) => !field.hidden).slice(0, 30).map((field) => ({
      name: field.fieldname,
      label: field.label ?? field.fieldname,
      type: fieldType(field.fieldtype),
    })),
  }))
}
