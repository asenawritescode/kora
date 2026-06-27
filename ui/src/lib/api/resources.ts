import { api } from './client'
import type { ListParams, ListResponse } from '@/types/api'
import type { Document } from '@/types/kora'

export async function fetchList(
  doctype: string,
  params: ListParams = {},
): Promise<ListResponse<Document>> {
  const queryParams: Record<string, string | number | undefined> = {
    limit: params.limit ?? 50,
    offset: params.offset ?? 0,
  }
  if (params.order_by) queryParams.order_by = params.order_by
  if (params.filters) queryParams.filters = params.filters
  if (params.fields) queryParams.fields = JSON.stringify(params.fields)

  // Use getEnvelope to preserve meta (total, doctype).
  const result = await api.getEnvelope<Document[]>(
    `/api/v1/resource/${encodeURIComponent(doctype)}`,
    queryParams,
  )
  return {
    data: result.data ?? [],
    meta: {
      doctype: result.meta?.doctype || doctype,
      total: result.meta?.total ?? 0,
    },
  }
}

export async function fetchDocument(doctype: string, name: string): Promise<Document> {
  return api.get<Document>(
    `/api/v1/resource/${encodeURIComponent(doctype)}/${encodeURIComponent(name)}`,
  )
}

export async function createDocument(doctype: string, data: Record<string, any>): Promise<Document> {
  return api.post<Document>(`/api/v1/resource/${encodeURIComponent(doctype)}`, data)
}

export async function updateDocument(
  doctype: string,
  name: string,
  data: Record<string, any>,
): Promise<Document> {
  return api.put<Document>(
    `/api/v1/resource/${encodeURIComponent(doctype)}/${encodeURIComponent(name)}`,
    data,
  )
}

export async function deleteDocument(doctype: string, name: string): Promise<void> {
  return api.delete(`/api/v1/resource/${encodeURIComponent(doctype)}/${encodeURIComponent(name)}`)
}

export async function submitWorkflowAction(
  doctype: string,
  name: string,
  action: string,
): Promise<Document> {
  return api.post<Document>(
    `/api/v1/resource/${encodeURIComponent(doctype)}/${encodeURIComponent(name)}/workflow_action`,
    { action },
  )
}
