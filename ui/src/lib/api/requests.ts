import {
  decodeContract,
  isContract,
  type CancelRequest,
  type InboxView,
  type PageRequest,
  type ReassignRequest,
  type RequestHistoryEntry,
  type RequestKind,
  type RequestView,
  type RespondRequest,
  type ResponseDetail,
  type ResponseReceipt,
  type SubmitRequest,
} from '@/generated/engine-v2'
import { api } from './client'

export interface RequestPage<T> {
  items: T[]
  next_cursor?: string
}

export interface RequestListOptions {
  view?: InboxView
  kind?: RequestKind
  limit?: number
  cursor?: string
  overdueOnly?: boolean
}

export async function fetchRequests(options: RequestListOptions = {}): Promise<RequestPage<RequestView>> {
  const page = await api.get<unknown>('/api/v2/requests', {
    view: options.view || 'needs_action',
    kind: options.kind || 'task',
    limit: options.limit || 25,
    cursor: options.cursor,
    overdue_only: String(options.overdueOnly || false),
  })
  return decodePage(page, 'RequestView')
}

export async function fetchRequest(id: string): Promise<RequestView> {
  return decodeContract<RequestView>('RequestView', await api.get<unknown>(`/api/v2/requests/${encodeURIComponent(id)}`))
}

export async function submitRequest(command: SubmitRequest): Promise<RequestView> {
  decodeContract<SubmitRequest>('SubmitRequest', command)
  return decodeContract<RequestView>('RequestView', await api.post<unknown>('/api/v2/requests', command))
}

export async function respondToRequest(id: string, command: RespondRequest): Promise<ResponseReceipt> {
  decodeContract<RespondRequest>('RespondRequest', command)
  return decodeContract<ResponseReceipt>('ResponseReceipt', await api.post<unknown>(`/api/v2/requests/${encodeURIComponent(id)}/responses`, command))
}

export async function reassignRequest(id: string, command: ReassignRequest): Promise<RequestView> {
  decodeContract<ReassignRequest>('ReassignRequest', command)
  return decodeContract<RequestView>('RequestView', await api.post<unknown>(`/api/v2/requests/${encodeURIComponent(id)}/reassignments`, command))
}

export async function cancelRequest(id: string, command: CancelRequest): Promise<RequestView> {
  decodeContract<CancelRequest>('CancelRequest', command)
  return decodeContract<RequestView>('RequestView', await api.post<unknown>(`/api/v2/requests/${encodeURIComponent(id)}/cancel`, command))
}

export async function fetchRequestHistory(id: string, page: PageRequest = { limit: 25 }): Promise<RequestPage<RequestHistoryEntry>> {
  const result = await api.get<unknown>(`/api/v2/requests/${encodeURIComponent(id)}/history`, {
    limit: page.limit,
    cursor: page.cursor,
  })
  return decodePage(result, 'RequestHistoryEntry')
}

export async function fetchRequestResponse(id: string, decisionId: string): Promise<ResponseDetail> {
  return decodeContract<ResponseDetail>('ResponseDetail', await api.get<unknown>(`/api/v2/requests/${encodeURIComponent(id)}/responses/${encodeURIComponent(decisionId)}`))
}

export async function markRequestRead(id: string): Promise<void> {
  await api.post<void>(`/api/v2/requests/${encodeURIComponent(id)}/read`)
}

export function idempotencyKey(): string {
  return crypto.randomUUID()
}

function decodePage<T>(value: unknown, itemName: 'RequestView' | 'RequestHistoryEntry'): RequestPage<T> {
  if (!value || typeof value !== 'object' || Array.isArray(value)) throw new TypeError(`Invalid Kora contract: Page<${itemName}>`)
  const page = value as { items?: unknown; next_cursor?: unknown }
  if (!Array.isArray(page.items) || !page.items.every(item => isContract(itemName, item)) ||
      (page.next_cursor !== undefined && typeof page.next_cursor !== 'string')) {
    throw new TypeError(`Invalid Kora contract: Page<${itemName}>`)
  }
  return page as RequestPage<T>
}
