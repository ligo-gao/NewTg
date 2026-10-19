/**
 * FE-08 settings transport.
 *
 * The generated OpenAPI schema does not yet carry `/api/settings`
 * (the settings registry lands in a parallel backend task), so this module
 * speaks the frozen contract directly while reusing the same CSRF,
 * idempotency and Problem-normalization helpers as the contract client.
 *
 * Frozen contract (m6c-task-1 brief):
 *   GET /api/settings?section=<section>  -> {section, updated_at, items[]}
 *   PUT /api/settings?section=<section>  -> 200 / 409 / 400 / 403
 */
import {
  CSRF_HEADER,
  getCsrfToken,
  toApiError,
} from '@/api/client'
import { ApiError } from '@/api/errors'

export interface SettingsItem {
  key: string
  /** Masked (`"******"`) for secret keys; `null` when never configured. */
  value: unknown
  type: string
  default: unknown
  public: boolean
  secret: boolean
  dangerous: boolean
}

export interface SettingsSectionResponse {
  section: string
  /** Optimistic-lock token; echoed back as `expect_updated_at`. */
  updated_at: string | null
  items: SettingsItem[]
}

export interface SettingsSectionWrite {
  section: string
  values: Record<string, unknown>
  expect_updated_at: string | null
  /** Set once the user confirmed the dangerous-change dialog. */
  confirmed_by?: { confirmed_at: string; session: string }
}

export interface ProblemFieldError {
  field: string
  message: string
}

/**
 * The settings handlers return field-level errors in one of two Problem
 * extensions: the shared `fields: string[]` list or the `errors:
 * [{field,message}]` array used by older management handlers. Keep both
 * normalized under one property so callers never branch on transport shape.
 */
export interface SettingsApiError extends Omit<ApiError, 'fields'> {
  fieldErrors?: ProblemFieldError[]
}

function apiUrl(path: string): string {
  if (typeof location === 'undefined') return path
  return new URL(path, location.origin).toString()
}

async function readFieldErrors(response: Response): Promise<ProblemFieldError[]> {
  try {
    const body: unknown = await response.clone().json()
    if (typeof body !== 'object' || body === null) return []
    const record = body as Record<string, unknown>

    const errors = record.errors
    if (Array.isArray(errors)) {
      return errors.flatMap((entry) => {
        if (typeof entry !== 'object' || entry === null) return []
        const item = entry as Record<string, unknown>
        if (typeof item.field !== 'string' || typeof item.message !== 'string') {
          return []
        }
        return [{ field: item.field, message: item.message }]
      })
    }

    const fields = record.fields
    if (Array.isArray(fields)) {
      return fields.flatMap((field) =>
        typeof field === 'string' && field.length > 0
          ? [{ field, message: '' }]
          : [],
      )
    }
    return []
  } catch {
    return []
  }
}

async function normalizeError(response: Response): Promise<SettingsApiError> {
  // `toApiError` consumes its own clone; read the field list from another
  // clone first so the original body is still available for both readers.
  const fieldErrors = await readFieldErrors(response)
  const error = await toApiError(response)
  return fieldErrors.length > 0
    ? Object.assign(error, { fieldErrors })
    : error
}

/** True when the error carries per-field validation detail. */
export function fieldErrorsOf(error: unknown): ProblemFieldError[] {
  if (error instanceof ApiError) {
    return (error as SettingsApiError).fieldErrors ?? []
  }
  return []
}

export async function fetchSettingsSection(
  section: string,
  signal?: AbortSignal,
): Promise<SettingsSectionResponse> {
  const url = new URL(apiUrl('/api/settings'))
  url.searchParams.set('section', section)
  const response = await fetch(url, {
    method: 'GET',
    credentials: 'same-origin',
    headers: { Accept: 'application/json' },
    signal,
  })
  if (!response.ok) throw await normalizeError(response)
  return (await response.json()) as SettingsSectionResponse
}

export async function saveSettingsSection(
  input: SettingsSectionWrite,
  idempotencyKey: string,
): Promise<SettingsSectionResponse> {
  const headers: Record<string, string> = {
    Accept: 'application/json',
    'Content-Type': 'application/json',
    'Idempotency-Key': idempotencyKey,
  }
  const csrfToken = getCsrfToken()
  if (csrfToken !== undefined) headers[CSRF_HEADER] = csrfToken

  const response = await fetch(apiUrl(`/api/settings?section=${encodeURIComponent(input.section)}`), {
    method: 'PUT',
    credentials: 'same-origin',
    headers,
    body: JSON.stringify(input),
  })
  if (!response.ok) throw await normalizeError(response)
  return (await response.json()) as SettingsSectionResponse
}

/** Minted once per save intent so a retried request is replayed, not doubled. */
export function createIdempotencyKey(): string {
  if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
    return crypto.randomUUID()
  }
  return `${Date.now()}-${Math.random().toString(16).slice(2)}`
}
