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
  toApiError,
} from '@/api/client'
import { ApiError, parseProblem } from '@/api/errors'

import type {
  BatchOperationResult,
  BatchStatusAction,
  Channel,
  ChannelListParams,
  ChannelListResponse,
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

export type ChannelApiError = ApiError & { fields?: ProblemFieldError[] }

/** Returns the field-level `errors` array when the problem body carried one. */
export function fieldErrorsOf(error: unknown): ProblemFieldError[] {
  if (error instanceof ApiError) {
    return (error as ChannelApiError).fields ?? []
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

function newIdempotencyKey(): string {
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
  const fields = await problemFields(response)
  return fields.length > 0 ? Object.assign(error, { fields }) : error
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

  let response: Response
  try {
    response = await fetch(apiUrl(path), {
      method,
      headers,
      body,
      credentials: 'same-origin',
      signal: options.signal,
    })
  } catch (error) {
    // Aborts are caller-controlled cancellation, not transport failures.
    if (error instanceof DOMException && error.name === 'AbortError') throw error
    throw parseProblem({ status: 0 })
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

export function createChannel(input: CreateChannelInput): Promise<Channel> {
  // POST /api/channels is idempotent (Task 8); the key is minted once per
  // user intent, and the caller never retries writes automatically.
  return request<Channel>('/api/channels', {
    method: 'POST',
    body: input,
    idempotencyKey: newIdempotencyKey(),
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

export function batchDeleteChannels(ids: number[]): Promise<BatchOperationResult> {
  return request<BatchOperationResult>('/api/channels/batch-delete', {
    method: 'POST',
    body: { ids },
    idempotencyKey: newIdempotencyKey(),
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
