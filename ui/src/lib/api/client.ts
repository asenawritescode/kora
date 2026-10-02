import type { ApiResponse, ApiErrorResponse, OperationEnvelope, OperationStatus } from '@/types/api'
import { getApiErrorMessage, getApiErrorCode } from './error-messages'
import { sitePath } from '../basepath'
import { loadRuntimeConfig } from '../runtime-config'

function getCsrfToken(): string {
  const match = document.cookie.match(/(?:^|;\s*)kora_csrf=([^;]*)/)
  return match ? decodeURIComponent(match[1]) : ''
}

function buildApiURL(path: string): URL {
  const runtime = loadRuntimeConfig()
  const apiBase = runtime.apiBaseUrl.replace(/\/$/, '')
  const requestPath = path.startsWith('/api/') ? path.slice(4) : path
  const target = `${apiBase}${requestPath}`
  const isAbsolute = /^https?:\/\//i.test(apiBase)
  // A path-routed workspace must keep its site prefix on every API request.
  // Otherwise localhost falls back to the globally scoped kora_site cookie,
  // which may still point at a different site opened earlier in the browser.
  return new URL(isAbsolute ? target : sitePath(target), window.location.origin)
}

export class KoraApiError extends Error {
  code: string
  field?: string
  details?: Record<string, unknown>
  status: number

  constructor(message: string, code: string, status: number, field?: string, details?: Record<string, unknown>) {
    super(message)
    this.name = 'KoraApiError'
    this.code = code
    this.status = status
    this.field = field
    this.details = details
  }
}

interface RequestOptions {
  params?: Record<string, string | number | undefined>
  headers?: Record<string, string>
}

async function apiRequest<T>(
  method: string,
  path: string,
  body?: unknown,
  params?: Record<string, string | number | undefined>,
): Promise<T> {
  const url = buildApiURL(path)
  if (params) {
    for (const [key, value] of Object.entries(params)) {
      if (value !== undefined) {
        url.searchParams.set(key, String(value))
      }
    }
  }

  const headers: Record<string, string> = {
    'Content-Type': 'application/json',
  }

  // CSRF token for state-changing requests.
  if (method !== 'GET' && method !== 'HEAD' && method !== 'OPTIONS') {
    const csrf = getCsrfToken()
    if (csrf) {
      headers['X-Kora-CSRF-Token'] = csrf
    }
  }

  const response = await fetch(url.toString(), {
    method,
    headers,
    credentials: 'same-origin',
    body: body ? JSON.stringify(body) : undefined,
  })

  if (!response.ok) {
    let errorData: ApiErrorResponse
    try {
      errorData = await response.json()
    } catch {
      throw new KoraApiError(
        `Request failed with status ${response.status}`,
        'network_error',
        response.status,
      )
    }

    // Plain string error: {"error": "some message"}
    if (typeof errorData.error === 'string') {
      throw new KoraApiError(errorData.error, 'error', response.status)
    }
    // Single error: {"error": {"code": "...", "message": "...", "field": "..."}}
    if (errorData.error && typeof errorData.error === 'object' && 'message' in errorData.error) {
      const err = errorData.error as any
      const code = getApiErrorCode(err)
      throw new KoraApiError(getApiErrorMessage(err), code, response.status, err.field, err.details)
    }
    // Multiple errors: {"error": {"errors": [...]}}
    if (errorData.error && typeof errorData.error === 'object' && 'errors' in errorData.error) {
      const err = (errorData.error as any).errors[0]
      const code = getApiErrorCode(err)
      throw new KoraApiError(getApiErrorMessage(err), code, response.status, err.field, err.details)
    }
    throw new KoraApiError('Unknown error', 'unknown', response.status)
  }

  // No content.
  if (response.status === 204) {
    return undefined as T
  }

  const json: ApiResponse<T> = await response.json()
  return json.data
}

export async function apiRequestOperation<T>(
  method: string,
  path: string,
  body?: unknown,
  params?: Record<string, string | number | undefined>,
  idempotencyKey: string = crypto.randomUUID(),
): Promise<OperationEnvelope<T>> {
  try {
    const { response, json } = await rawApiRequest<unknown>(method, path, body, {
      params,
      headers: { 'Idempotency-Key': idempotencyKey },
    })
    return normalizeOperationEnvelope<T>(json, response, idempotencyKey)
  } catch (error) {
    if (error instanceof KoraApiError) {
      return {
        operation_id: idempotencyKey,
        status: mapErrorStatus(error.status),
        error: {
          code: error.code || 'error',
          message: error.message,
          field: error.field,
          retryable: error.status >= 500,
        },
      }
    }
    throw error
  }
}

// Convenience methods.
export const api = {
  get<T>(path: string, params?: Record<string, string | number | undefined>) {
    return apiRequest<T>('GET', path, undefined, params)
  },
  // Returns the full response envelope including meta.
  getEnvelope<T>(path: string, params?: Record<string, string | number | undefined>) {
    return apiRequestEnvelope<T>('GET', path, undefined, params)
  },
  post<T>(path: string, body?: unknown) {
    return apiRequest<T>('POST', path, body)
  },
  put<T>(path: string, body?: unknown) {
    return apiRequest<T>('PUT', path, body)
  },
  delete<T>(path: string) {
    return apiRequest<T>('DELETE', path)
  },
  operation<T>(
    method: 'POST' | 'PUT' | 'DELETE',
    path: string,
    body?: unknown,
    params?: Record<string, string | number | undefined>,
    idempotencyKey?: string,
  ) {
    return apiRequestOperation<T>(method, path, body, params, idempotencyKey)
  },
}

// Like apiRequest but returns the full envelope.
async function apiRequestEnvelope<T>(
  method: string,
  path: string,
  body?: unknown,
  params?: Record<string, string | number | undefined>,
): Promise<{ data: T; meta?: { total?: number; doctype?: string; config_version?: number; capabilities_version?: string; next_cursor?: string | null; has_more?: boolean } }> {
  const url = buildApiURL(path)
  if (params) {
    for (const [key, value] of Object.entries(params)) {
      if (value !== undefined) {
        url.searchParams.set(key, String(value))
      }
    }
  }

  const headers: Record<string, string> = { 'Content-Type': 'application/json' }
  if (method !== 'GET' && method !== 'HEAD' && method !== 'OPTIONS') {
    const csrf = getCsrfToken()
    if (csrf) headers['X-Kora-CSRF-Token'] = csrf
  }

  const response = await fetch(url.toString(), {
    method,
    headers,
    credentials: 'same-origin',
    body: body ? JSON.stringify(body) : undefined,
  })

  if (!response.ok) {
    let errorData: ApiErrorResponse
    try { errorData = await response.json() } catch {
      throw new KoraApiError(`Request failed with status ${response.status}`, 'network_error', response.status)
    }
    const err = (errorData.error as any)
    if (err?.message || err?.code || err?.type) {
      const code = getApiErrorCode(err)
      throw new KoraApiError(getApiErrorMessage(err), code, response.status, err.field, err.details)
    }
    if (err?.errors?.[0]) {
      const first = err.errors[0]
      const code = getApiErrorCode(first)
      throw new KoraApiError(getApiErrorMessage(first), code, response.status, first.field, first.details)
    }
    throw new KoraApiError('Unknown error', 'unknown', response.status)
  }

  return response.json()
}

async function rawApiRequest<T>(
  method: string,
  path: string,
  body?: unknown,
  options: RequestOptions = {},
): Promise<{ response: Response; json: T | undefined }> {
  const url = buildApiURL(path)
  if (options.params) {
    for (const [key, value] of Object.entries(options.params)) {
      if (value !== undefined) {
        url.searchParams.set(key, String(value))
      }
    }
  }

  const headers: Record<string, string> = {
    'Content-Type': 'application/json',
    ...options.headers,
  }
  if (method !== 'GET' && method !== 'HEAD' && method !== 'OPTIONS') {
    const csrf = getCsrfToken()
    if (csrf) headers['X-Kora-CSRF-Token'] = csrf
  }

  const response = await fetch(url.toString(), {
    method,
    headers,
    credentials: 'same-origin',
    body: body ? JSON.stringify(body) : undefined,
  })

  if (!response.ok) {
    throw await toApiError(response)
  }

  if (response.status === 204) {
    return { response, json: undefined }
  }

  return { response, json: await response.json() as T }
}

async function toApiError(response: Response): Promise<KoraApiError> {
  let errorData: ApiErrorResponse
  try {
    errorData = await response.json()
  } catch {
    return new KoraApiError(`Request failed with status ${response.status}`, 'network_error', response.status)
  }
  if (typeof errorData.error === 'string') {
    return new KoraApiError(errorData.error, 'error', response.status)
  }
  if (errorData.error && typeof errorData.error === 'object' && 'message' in errorData.error) {
    const err = errorData.error as any
    const code = getApiErrorCode(err)
    return new KoraApiError(getApiErrorMessage(err), code, response.status, err.field, err.details)
  }
  if (errorData.error && typeof errorData.error === 'object' && 'errors' in errorData.error) {
    const err = (errorData.error as any).errors[0]
    const code = getApiErrorCode(err)
    return new KoraApiError(getApiErrorMessage(err), code, response.status, err.field, err.details)
  }
  return new KoraApiError('Unknown error', 'unknown', response.status)
}

function normalizeOperationEnvelope<T>(
  json: unknown,
  response: Response,
  idempotencyKey: string,
): OperationEnvelope<T> {
  if (isOperationEnvelope<T>(json)) {
    return withOperationDefaults(json, response, idempotencyKey)
  }

  const apiResponse = json as ApiResponse<unknown> | undefined
  if (isOperationEnvelope<T>(apiResponse?.data)) {
    return withOperationDefaults(apiResponse.data, response, idempotencyKey)
  }

  return {
    operation_id: readHeader(response, 'X-Kora-Operation-Id') || idempotencyKey,
    correlation_id: readHeader(response, 'X-Kora-Correlation-Id'),
    status: response.status === 202 ? 'accepted' : 'completed',
    data: apiResponse && 'data' in apiResponse ? apiResponse.data as T : json as T,
  }
}

function withOperationDefaults<T>(
  envelope: OperationEnvelope<T>,
  response: Response,
  idempotencyKey: string,
): OperationEnvelope<T> {
  return {
    ...envelope,
    operation_id: envelope.operation_id || readHeader(response, 'X-Kora-Operation-Id') || idempotencyKey,
    correlation_id: envelope.correlation_id || readHeader(response, 'X-Kora-Correlation-Id'),
  }
}

function isOperationEnvelope<T>(value: unknown): value is OperationEnvelope<T> {
  if (!value || typeof value !== 'object') return false
  const status = (value as { status?: unknown }).status
  return typeof (value as { operation_id?: unknown }).operation_id === 'string' &&
    typeof status === 'string' &&
    ['completed', 'accepted', 'rejected', 'conflict', 'failed'].includes(status)
}

function readHeader(response: Response, header: string): string | undefined {
  return response.headers.get(header) || undefined
}

function mapErrorStatus(status: number): OperationStatus {
  if (status === 409) return 'conflict'
  if (status === 400 || status === 401 || status === 403 || status === 422) return 'rejected'
  return 'failed'
}
