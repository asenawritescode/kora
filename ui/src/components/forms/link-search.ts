import type { DocType } from '@/types/kora'

export function linkPickerDisplayFields(doctype?: Pick<DocType, 'title_field' | 'search_fields'>): string[] {
  return [...new Set([
    'name',
    doctype?.title_field?.trim() || 'name',
    ...(doctype?.search_fields || '').split(',').map((field) => field.trim()).filter(Boolean),
  ])]
}

export function linkPickerSearchParams(term: string, fields: string[]) {
  return { limit: 10, fields, search: term }
}

