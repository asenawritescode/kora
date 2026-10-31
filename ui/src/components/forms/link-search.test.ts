import { describe, expect, it } from 'vitest'
import { linkPickerDisplayFields, linkPickerSearchParams } from './link-search'

describe('link picker database search contract', () => {
  it('includes the readable title and configured fields without duplicates', () => {
    expect(linkPickerDisplayFields({ title_field: 'full_name', search_fields: 'first_name, email, full_name' }))
      .toEqual(['name', 'full_name', 'first_name', 'email'])
  })

  it('sends the visible title and configured display fields to bounded Engine search', () => {
    const fields = linkPickerDisplayFields({ title_field: 'company_name', search_fields: 'website, industry' })
    expect(linkPickerSearchParams('Northwind', fields)).toEqual({
      limit: 10,
      fields: ['name', 'company_name', 'website', 'industry'],
      search: 'Northwind',
    })
    expect(linkPickerSearchParams('Northwind', fields)).not.toHaveProperty('filters')
  })
})
