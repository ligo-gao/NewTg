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
  /**
   * Server-declared risky direction (contract v2): `off` means turning the
   * key false needs confirmation, `on` means turning it true does. `null` /
   * absent means the server did not classify the direction and the client
   * falls back to its local table.
   */
  danger_direction?: 'off' | 'on' | null
}

export interface SettingsSectionResponse {
  section: string
  /** Optimistic-lock token; echoed back as `expect_updated_at`. */
  updated_at: string | null
  items: SettingsItem[]
}

export interface SettingsValuesResponse {
  section: string
  updated_at: string | null
  /** Masked values exactly as stored after the write. */
  values: Record<string, unknown>
}

export interface SettingsSectionWrite {
  section: string
  values: Record<string, unknown>
  expect_updated_at: string | null
  /**
   * Contract v2: set only after the dangerous-change dialog is accepted.
   * The backend whitelists it and maps it to UpdateInput.DangerousConfirmed.
   */
  dangerous_confirmed?: true
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

    const merged: ProblemFieldError[] = []
    const push = (field: string, message: string): void => {
      if (!merged.some((entry) => entry.field === field)) {
        merged.push({ field, message })
      }
    }

    const errors = record.errors
    if (Array.isArray(errors)) {
      for (const entry of errors) {
        if (typeof entry !== 'object' || entry === null) continue
        const item = entry as Record<string, unknown>
        if (typeof item.field === 'string' && typeof item.message === 'string') {
          push(item.field, item.message)
        }
      }
    }

    // Contract v2 keeps `fields: string[]` as the shared Problem extension;
    // merge it even when `errors` exists (including an empty array) so a
    // handler that sends only `fields` still produces inline errors.
    const fields = record.fields
    if (Array.isArray(fields)) {
      for (const field of fields) {
        if (typeof field === 'string' && field.length > 0) push(field, '')
      }
    }
    return merged
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
): Promise<SettingsValuesResponse> {
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
  return (await response.json()) as SettingsValuesResponse
}

/** Minted once per save intent so a retried request is replayed, not doubled. */
export function createIdempotencyKey(): string {
  if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
    return crypto.randomUUID()
  }
  return `${Date.now()}-${Math.random().toString(16).slice(2)}`
}
