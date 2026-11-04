import { useState } from 'react'
import type { ViewComponentProps } from './registry'

/**
 * CategoryTabs renders horizontal category filter tabs.
 * Tapping a category filters the sibling product_grid component.
 */
export default function CategoryTabs(props: ViewComponentProps) {
  const { data, config, onAction } = props
  const [selected, setSelected] = useState('')

  const categories = data?.data || []
  const groupField = config.bindings?.group_field
  if (!groupField) {
    return <div className="text-sm text-muted-foreground">Configure a category field binding.</div>
  }
  const categoryRecords = props.resources?.categories?.data?.data
  const uniqueCategories = categoryTabsFromResources(
    categories,
    groupField,
    Array.isArray(categoryRecords) ? categoryRecords : [],
    config.bindings?.category_label_field,
  )

  if (props.resources?.categories?.kind === 'loading') {
    return <div className="flex gap-2 overflow-x-auto pb-2" role="status" aria-live="polite">
      <button type="button" aria-pressed={!selected} className="px-3 py-1.5 text-sm rounded-full bg-primary text-primary-foreground whitespace-nowrap"
        onClick={() => { setSelected(''); onAction('filter', { field: groupField, value: '', doctype: config.source_doctype }) }}>All</button>
      <span className="px-3 py-1.5 text-sm text-muted-foreground">Loading categories…</span>
    </div>
  }

  if (uniqueCategories.length === 0) {
    return <div className="flex gap-2 overflow-x-auto pb-2">
      {['All'].map(cat => (
        <button key={cat} type="button" className="px-3 py-1.5 text-sm rounded-full bg-primary text-primary-foreground whitespace-nowrap"
          onClick={() => { setSelected(''); onAction('filter', { field: groupField, value: '' }) }}>{cat}</button>
      ))}
    </div>
  }

  return (
    <div className="flex gap-2 overflow-x-auto pb-2">
      <button type="button" className={`px-3 py-1.5 text-sm rounded-full whitespace-nowrap ${selected === '' ? 'bg-primary text-primary-foreground' : 'bg-muted hover:bg-muted/80'}`}
        onClick={() => { setSelected(''); onAction('filter', { field: groupField, value: '' }) }}>All</button>
      {uniqueCategories.map(({ value, label }) => (
        <button key={value} type="button" aria-pressed={selected === value} className={`px-3 py-1.5 text-sm rounded-full whitespace-nowrap ${selected === value ? 'bg-primary text-primary-foreground' : 'bg-muted hover:bg-muted/80'}`}
          onClick={() => { setSelected(value); onAction('filter', { field: groupField, value, doctype: config.source_doctype }) }}>{label}</button>
      ))}
    </div>
  )
}

export function categoryTabsFromResources(
  products: Array<Record<string, unknown>>,
  groupField: string,
  categories: Array<Record<string, unknown>> = [],
  labelField = '',
): Array<{ value: string; label: string }> {
  const labelsByID = new Map(categories.flatMap((category) => {
    const key = category.name == null ? '' : String(category.name).trim()
    const label = labelField && category[labelField] != null ? String(category[labelField]).trim() : ''
    return key && label ? [[key, label] as const] : []
  }))
  const values = [...new Set(products
    .map((product) => product[groupField])
    .filter((value): value is string | number => typeof value === 'string' || typeof value === 'number')
    .map((value) => String(value).trim())
    .filter(Boolean))]
  return values.map((value) => ({ value, label: labelsByID.get(value) || value }))
}
