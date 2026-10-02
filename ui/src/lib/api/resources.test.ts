import { beforeEach, describe, expect, it, vi } from 'vitest'

vi.mock('./client', () => ({
  api: {
    get: vi.fn(),
    getEnvelope: vi.fn(),
    post: vi.fn(),
    put: vi.fn(),
    delete: vi.fn(),
  },
}))

import { api } from './client'
import {
  createDocument,
  deleteDocument,
  fetchDocument,
  fetchList,
  submitWorkflowAction,
  updateDocument,
} from './resources'

describe('MVP resource client contract', () => {
  beforeEach(() => vi.clearAllMocks())

  it('uses the existing resource routes and unwraps the document contract', async () => {
    const document = { name: 'PROD-0001', product_name: 'Consultation' }
    vi.mocked(api.getEnvelope).mockResolvedValueOnce({
      data: [document],
      meta: { doctype: 'Product', total: 1 },
    })
    vi.mocked(api.get).mockResolvedValueOnce(document)
    vi.mocked(api.post).mockResolvedValueOnce(document)
    vi.mocked(api.put).mockResolvedValueOnce(document)
    vi.mocked(api.delete).mockResolvedValueOnce(undefined)

    await expect(fetchList('Product', { limit: 25, offset: 5 })).resolves.toEqual({
      data: [document], meta: { doctype: 'Product', total: 1 },
    })
    await expect(fetchDocument('Product', document.name)).resolves.toEqual(document)
    await expect(createDocument('Product', { product_name: 'Consultation' })).resolves.toEqual(document)
    await expect(updateDocument('Product', document.name, { product_name: 'Consultation' })).resolves.toEqual(document)
    await expect(deleteDocument('Product', document.name)).resolves.toBeUndefined()

    expect(api.getEnvelope).toHaveBeenCalledWith('/api/v1/resource/Product', { limit: 25, offset: 5 })
    expect(api.get).toHaveBeenCalledWith('/api/v1/resource/Product/PROD-0001')
    expect(api.post).toHaveBeenCalledWith('/api/v1/resource/Product', { product_name: 'Consultation' })
    expect(api.put).toHaveBeenCalledWith('/api/v1/resource/Product/PROD-0001', { product_name: 'Consultation' })
    expect(api.delete).toHaveBeenCalledWith('/api/v1/resource/Product/PROD-0001')
  })

  it('keeps workflow actions on the established route and response shape', async () => {
    const document = { name: 'SALE-0001', status: 'Paid' }
    vi.mocked(api.post).mockResolvedValueOnce(document)

    await expect(submitWorkflowAction('Sale', document.name, 'mark_paid')).resolves.toEqual(document)
    expect(api.post).toHaveBeenCalledWith(
      '/api/v1/resource/Sale/SALE-0001/workflow_action',
      { action: 'mark_paid' },
    )
  })
})
