/**
 * Channel management API (Task 8 endpoints).
 *
 * The generated `api/schema.d.ts` still carries only the M0 list stub, so this
 * module speaks to the management endpoints directly. It reuses the frozen
 * client helpers (`getCsrfToken`, `CSRF_HEADER`, `toApiError`) so writes carry
 * the same CSRF and idempotency headers as the contract client, while the
 * request/response shapes come from the Task 8 handlers.
 */
import {
  CSRF_HEADER,
  WRITE_METHODS,
  getCsrfToken,
  isCsrfFailure,
  refreshCsrfCookie,
  toApiError,
} from '@/api/client'
import { ApiError, parseProblem } from '@/api/errors'

import type {
  BatchOperationResult,
  BatchStatusAction,
  Channel,
  ChannelListParams,
  ChannelListResponse,
  ChannelMetrics,
  ChannelMetricsWindow,
  ChannelTestInput,
  ChannelTestResult,
  CreateChannelInput,
  UpdateChannelInput,
} from './types'

/** Allowlisted RFC 7807 extension used by the management 422 responses. */
export interface ProblemFieldError {
  field: string
  message: string
}

/**
 * M3's `ApiError` carries `fields?: string[]` from the RFC 7807
 * `problem.fields` extension. Channel management 422s use a different
 * extension (`errors: [{field, message}]`) normalised into
 * `ProblemFieldError[]`; keep it under a distinct property so the two shapes
 * can never be confused at runtime or intersect to `never` in the type system.
 */
export interface ChannelApiError extends Omit<ApiError, 'fields'> {
  fieldErrors?: ProblemFieldError[]
}

/** Returns the field-level `errors` array when the problem body carried one. */
export function fieldErrorsOf(error: unknown): ProblemFieldError[] {
  if (error instanceof ApiError) {
    return (error as ChannelApiError).fieldErrors ?? []
  }
  return []
}

/**
 * The app is same-origin; resolving against `location.origin` keeps `fetch`
 * absolute, which node-based test runners require for a valid Request URL.
 */
function apiUrl(path: string): string {
  if (typeof location === 'undefined') return path
  return new URL(path, location.origin).toString()
}

interface RequestOptions {
  method?: 'GET' | 'POST' | 'PATCH' | 'DELETE'
  body?: unknown
  idempotencyKey?: string
  signal?: AbortSignal
}

/**
 * Mints the key for one user intent. Callers that own the intent (the create
 * sheet, the bulk-delete confirmation) generate it on first submit and pass
 * the same value on retries so the backend can replay instead of duplicating.
 */
export function createIdempotencyKey(): string {
  if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
    return crypto.randomUUID()
  }
  return `${Date.now()}-${Math.random().toString(16).slice(2)}`
}

/**
 * Reads the 422 field list from a fresh clone: `toApiError` consumes its own
 * clone, so the original body is still available here. Only `field`/`message`
 * are copied (GC-6 allowlist) — no raw problem members reach the UI.
 */
async function problemFields(response: Response): Promise<ProblemFieldError[]> {
  try {
    const body: unknown = await response.clone().json()
    if (typeof body !== 'object' || body === null) return []
    const errors = (body as Record<string, unknown>).errors
    if (!Array.isArray(errors)) return []
    return errors.flatMap((entry) => {
      if (typeof entry !== 'object' || entry === null) return []
      const record = entry as Record<string, unknown>
      if (typeof record.field !== 'string' || typeof record.message !== 'string') {
        return []
      }
      return [{ field: record.field, message: record.message }]
    })
  } catch {
    return []
  }
}

async function normalizeError(response: Response): Promise<ChannelApiError> {
  const error = await toApiError(response)
  const fieldErrors = await problemFields(response)
  return fieldErrors.length > 0 ? Object.assign(error, { fieldErrors }) : error
}

async function dispatch(
  path: string,
  init: RequestInit,
): Promise<Response> {
  try {
    return await fetch(apiUrl(path), init)
  } catch (error) {
    // Aborts are caller-controlled cancellation, not transport failures.
    if (error instanceof DOMException && error.name === 'AbortError') throw error
    throw parseProblem({ status: 0 })
  }
}

async function request<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const method = options.method ?? 'GET'
  const headers = new Headers({ Accept: 'application/json' })

  if (WRITE_METHODS.has(method)) {
    const csrfToken = getCsrfToken()
    if (csrfToken !== undefined) headers.set(CSRF_HEADER, csrfToken)
    if (options.idempotencyKey !== undefined) {
      headers.set('Idempotency-Key', options.idempotencyKey)
    }
  }

  let body: string | undefined
  if (options.body !== undefined) {
    headers.set('Content-Type', 'application/json')
    body = JSON.stringify(options.body)
  }

  const init: RequestInit = {
    method,
    headers,
    body,
    credentials: 'same-origin',
    signal: options.signal,
  }

  let response = await dispatch(path, init)

  // Mirror the frozen contract client's recovery (security-01 §2.2, §3.5):
  // one `GET /api/me` refresh, then replay this write exactly once with the
  // refreshed cookie. A second failure is surfaced as `forbidden` below.
  if (!response.ok && WRITE_METHODS.has(method) && (await isCsrfFailure(response))) {
    await refreshCsrfCookie()
    headers.delete(CSRF_HEADER)
    const refreshedToken = getCsrfToken()
    if (refreshedToken !== undefined) headers.set(CSRF_HEADER, refreshedToken)
    response = await dispatch(path, init)
  }

  if (!response.ok) throw await normalizeError(response)
  if (response.status === 204) return undefined as T
  return (await response.json()) as T
}

function channelQuery(params: ChannelListParams): string {
  const query = new URLSearchParams()
  query.set('p', String(params.page))
  query.set('page_size', String(params.pageSize))
  if (params.keyword !== '') query.set('keyword', params.keyword)
  if (params.status !== null) query.set('status', String(params.status))
  for (const type of params.types) query.append('type[]', type)
  if (params.group !== '') query.set('group', params.group)
  if (params.model !== '') query.set('model', params.model)
  query.set('sort', params.sort)
  return query.toString()
}

export function listChannels(params: ChannelListParams): Promise<ChannelListResponse> {
  return request<ChannelListResponse>(`/api/channels?${channelQuery(params)}`)
}

export function getChannel(id: number): Promise<Channel> {
  return request<Channel>(`/api/channels/${id}`)
}

/**
 * GET /api/channels/{id}/metrics (W16-1 / F-06-11).
 *
 * `signal` lets React Query cancel the in-flight request when the user flips
 * the 24h/7d tab or collapses the row; the backend caches the aggregate for
 * 60s under `newtg:channel_metrics:<id>:<window>:1h`, so a re-open is cheap.
 */
export function getChannelMetrics(
  id: number,
  window: ChannelMetricsWindow,
  signal?: AbortSignal,
): Promise<ChannelMetrics> {
  const query = new URLSearchParams({ window })
  return request<ChannelMetrics>(`/api/channels/${id}/metrics?${query.toString()}`, {
    signal,
  })
}

/**
 * POST /api/channels is idempotent (Task 8). The caller owns the key so a
 * retry of the same intent (same payload) replays instead of creating a
 * second channel; only callers without retry state fall back to a fresh key.
 */
export function createChannel(
  input: CreateChannelInput,
  idempotencyKey?: string,
): Promise<Channel> {
  return request<Channel>('/api/channels', {
    method: 'POST',
    body: input,
    idempotencyKey: idempotencyKey ?? createIdempotencyKey(),
  })
}

export function updateChannel(
  id: number,
  input: UpdateChannelInput,
): Promise<Channel> {
  return request<Channel>(`/api/channels/${id}`, {
    method: 'PATCH',
    body: input,
  })
}

export function deleteChannel(id: number): Promise<void> {
  return request<void>(`/api/channels/${id}`, { method: 'DELETE' })
}

export function batchChannelStatus(
  action: BatchStatusAction,
  ids: number[],
): Promise<BatchOperationResult> {
  return request<BatchOperationResult>('/api/channels/batch', {
    method: 'PATCH',
    body: { action, ids },
  })
}

/**
 * POST /api/channels/batch-delete is idempotent (Task 8); like `createChannel`
 * the key belongs to the confirmation intent so a retry with the same id set
 * replays rather than deleting a second time.
 */
export function batchDeleteChannels(
  ids: number[],
  idempotencyKey?: string,
): Promise<BatchOperationResult> {
  return request<BatchOperationResult>('/api/channels/batch-delete', {
    method: 'POST',
    body: { ids },
    idempotencyKey: idempotencyKey ?? createIdempotencyKey(),
  })
}

export function testChannel(
  id: number,
  input: ChannelTestInput,
): Promise<ChannelTestResult> {
  return request<ChannelTestResult>(`/api/channels/${id}/test`, {
    method: 'POST',
    body: input,
  })
}
