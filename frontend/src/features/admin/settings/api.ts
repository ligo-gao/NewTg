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
  isCsrfFailure,
  refreshCsrfCookie,
  toApiError,
} from '@/api/client'
import { ApiError, parseProblem } from '@/api/errors'

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
   * key false needs confirmation, `on` means turning it true does, and `any`
   * means any change is risky. `null` / absent means the server did not
   * classify the direction and the client falls back to its local table.
   */
  danger_direction?: 'off' | 'on' | 'any' | null
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

/** Response of `POST /api/settings/test/webhook` (D2-14). */
export interface WebhookTestResult {
  /** Target answered with a 2xx status. 3xx/4xx/5xx are all `false`. */
  ok: boolean
  /** Target status code passed through; `0` when no HTTP response arrived. */
  status_code: number
}

/**
 * Sends the fixed sample event to the **saved** `notify.webhook.*` snapshot.
 *
 * The endpoint is in the generated contract, but the surrounding settings
 * transport is hand-rolled (the section registry is not in the schema yet),
 * so this sits next to `fetchSettingsSection` and shares its Problem
 * normalization.
 *
 * Transport handling mirrors `features/admin/notifications/api.ts`:
 * - a `TypeError` from `fetch` is a transport failure, normalized to a
 *   status-0 Problem so the panel's offline branch is reachable;
 * - an `AbortError` is rethrown unchanged (an unmount/cancel is not a failure);
 * - `403 csrf_failed` refreshes the session cookie via `GET /api/me` and
 *   replays exactly once with the re-read token.
 * `Retry-After` on 429 reaches callers via `ApiError`.
 */
export async function testWebhook(signal?: AbortSignal): Promise<WebhookTestResult> {
  const response = await postSettingsTest('/api/settings/test/webhook', signal)
  if (!response.ok) throw await normalizeError(response)
  return (await response.json()) as WebhookTestResult
}

/** One SMTP stage; `steps` is ordered by execution and stops at the first failure. */
export interface SMTPTestStep {
  stage: 'dns' | 'connect' | 'tls' | 'greeting' | 'auth' | 'mail_from'
  status: 'passed' | 'failed' | 'skipped'
  /** Fixed server-side enum; never raw SMTP text. */
  reason?: string
  /** Numeric SMTP reply code (e.g. 535/550), when the server replied. */
  smtp_code?: number
}

/** Response of `POST /api/settings/test/smtp`. */
export interface SMTPTestResult {
  /** Reached MAIL FROM and the server accepted it. */
  ok: boolean
  steps: SMTPTestStep[]
}

/**
 * Probes the **saved** `notify.smtp.*` snapshot stage by stage.
 *
 * The probe never enters DATA, so no mail is delivered. A failed probe is
 * still HTTP 200 with `ok=false`; only 422 (not configured), 429 (rate
 * limited), 5xx and transport failures throw.
 */
export async function testSMTP(signal?: AbortSignal): Promise<SMTPTestResult> {
  const response = await postSettingsTest('/api/settings/test/smtp', signal)
  if (!response.ok) throw await normalizeError(response)
  return (await response.json()) as SMTPTestResult
}

/**
 * Shared POST for the administrator-only settings probes (webhook / SMTP).
 *
 * Transport handling mirrors `features/admin/notifications/api.ts`:
 * - a `TypeError` from `fetch` is a transport failure, normalized to a
 *   status-0 Problem so the panel's offline branch is reachable;
 * - an `AbortError` is rethrown unchanged (an unmount/cancel is not a failure);
 * - `403 csrf_failed` refreshes the session cookie via `GET /api/me` and
 *   replays exactly once with the re-read token.
 * `Retry-After` on 429 reaches callers via `ApiError`.
 */
async function postSettingsTest(path: string, signal?: AbortSignal): Promise<Response> {
  const headers = new Headers({ Accept: 'application/json' })
  const csrfToken = getCsrfToken()
  if (csrfToken !== undefined) headers.set(CSRF_HEADER, csrfToken)

  const init: RequestInit = {
    method: 'POST',
    credentials: 'same-origin',
    headers,
    signal,
  }

  const dispatch = async (): Promise<Response> => {
    try {
      return await fetch(apiUrl(path), init)
    } catch (error) {
      // 调用方主动取消不是传输失败，必须原样抛出让 React Query 丢弃结果。
      if (error instanceof DOMException && error.name === 'AbortError') throw error
      // Only a transport-level TypeError means "no HTTP response arrived";
      // programming errors are not offline.
      if (error instanceof TypeError) throw parseProblem({ status: 0 })
      throw error
    }
  }

  let response = await dispatch()

  if (!response.ok && (await isCsrfFailure(response))) {
    await refreshCsrfCookie()
    // The replay must not resend the rejected token when the refresh did not
    // produce a cookie (`init.headers` is the same Headers instance).
    headers.delete(CSRF_HEADER)
    const refreshedToken = getCsrfToken()
    if (refreshedToken !== undefined) headers.set(CSRF_HEADER, refreshedToken)
    response = await dispatch()
  }

  return response
}

/** Payment settings compliance status for the current administrator. */
export interface PaymentComplianceStatus {
  confirmed: boolean
  /** First confirmation time; null until confirmed. */
  confirmed_at: string | null
}

/** Shared cache key for the payment-tab compliance read. */
export const PAYMENT_COMPLIANCE_QUERY_KEY = ['settings', 'payment-compliance'] as const

export async function fetchPaymentCompliance(
  signal?: AbortSignal,
): Promise<PaymentComplianceStatus> {
  const response = await fetch(apiUrl('/api/settings/payment-compliance'), {
    method: 'GET',
    credentials: 'same-origin',
    headers: { Accept: 'application/json' },
    signal,
  })
  if (!response.ok) throw await normalizeError(response)
  return (await response.json()) as PaymentComplianceStatus
}

/**
 * Submits the exact confirmation text (modules-08 §3.4). The server re-checks
 * it character by character; the disabled button in the gate is only a local
 * usability aid. The endpoint is idempotent, so no Idempotency-Key is needed.
 */
export async function confirmPaymentCompliance(
  confirmation: string,
): Promise<PaymentComplianceStatus> {
  const headers: Record<string, string> = {
    Accept: 'application/json',
    'Content-Type': 'application/json',
  }
  const csrfToken = getCsrfToken()
  if (csrfToken !== undefined) headers[CSRF_HEADER] = csrfToken

  const response = await fetch(apiUrl('/api/settings/payment-compliance'), {
    method: 'POST',
    credentials: 'same-origin',
    headers,
    body: JSON.stringify({ confirmation }),
  })
  if (!response.ok) throw await normalizeError(response)
  return (await response.json()) as PaymentComplianceStatus
}

/** Minted once per save intent so a retried request is replayed, not doubled. */
export function createIdempotencyKey(): string {
  if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
    return crypto.randomUUID()
  }
  return `${Date.now()}-${Math.random().toString(16).slice(2)}`
}
