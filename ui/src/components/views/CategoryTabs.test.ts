import { describe, expect, it } from 'vitest'
import { categoryTabsFromResources } from './CategoryTabs'

describe('category tabs resource contract', () => {
  it('uses plain text values as both labels and filter values', () => {
    expect(categoryTabsFromResources(
      [{ category: 'Utensil' }, { category: 'Food' }, { category: 'Utensil' }],
      'category',
    )).toEqual([
      { value: 'Utensil', label: 'Utensil' },
      { value: 'Food', label: 'Food' },
    ])
  })

  it('uses labels from a declared category resource while preserving linked IDs', () => {
    expect(categoryTabsFromResources(
      [{ category: 'CAT-001' }, { category: 'CAT-002' }, { category: 'CAT-001' }],
      'category',
      [{ name: 'CAT-001', category_name: 'Kitchenware' }, { name: 'CAT-002', category_name: 'Food' }],
      'category_name',
    )).toEqual([
      { value: 'CAT-001', label: 'Kitchenware' },
      { value: 'CAT-002', label: 'Food' },
    ])
  })
})
