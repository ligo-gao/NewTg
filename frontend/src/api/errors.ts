/**
 * RFC 7807 `Problem` normalization (web-00 §5.2).
 *
 * Every non-2xx response and every transport failure is funnelled into a single
 * `ApiError` shape so callers can branch on `kind` rather than on raw status
 * codes. Only allowlisted Problem fields are copied — upstream credentials,
 * prompts and other payload fields must never reach the UI (GC-2).
 */

export type ApiErrorKind =
  | 'auth'
  | 'forbidden'
  | 'not_found'
  | 'conflict'
  | 'validation'
  | 'quota'
  | 'rate_limited'
  | 'server'
  | 'offline'

/**
 * `Problem402` extends `Problem` (platform-03 §6): the insufficient-credits
 * details travel next to the RFC 7807 members, not in a nested object.
 */
export interface Problem402 {
  balance: number
  required?: number
  currency?: string
  topup_url?: string
}

export interface ApiErrorFields {
  status: number
  type: string
  title: string
  detail?: string
  instance?: string
  problem402?: Problem402
  request_id?: string
  /**
   * Seconds from a `Retry-After` response header. Server-driven countdowns
   * (email-code 429, login lockout) must never hardcode their duration.
   */
  retry_after?: number
  kind: ApiErrorKind
}

/** Normalizes an HTTP status code to the vocabulary used by the UI. */
export function kindForStatus(status: number): ApiErrorKind {
  if (status === 0) return 'offline'
  if (status === 401) return 'auth'
  if (status === 402) return 'quota'
  if (status === 403) return 'forbidden'
  if (status === 404) return 'not_found'
  if (status === 409) return 'conflict'
  if (status === 400 || status === 422) return 'validation'
  if (status === 429) return 'rate_limited'
  if (status >= 500) return 'server'
  // Remaining 4xx statuses are still caller mistakes: treat them as validation
  // so retry policy (no automatic retry) remains correct.
  return status >= 400 ? 'validation' : 'server'
}

export class ApiError extends Error {
  readonly status: number
  readonly type: string
  readonly title: string
  readonly detail?: string
  readonly instance?: string
  readonly problem402?: Problem402
  readonly request_id?: string
  readonly retry_after?: number
  readonly kind: ApiErrorKind

  constructor(fields: ApiErrorFields) {
    super(fields.detail ?? fields.title)
    this.name = 'ApiError'
    this.status = fields.status
    this.type = fields.type
    this.title = fields.title
    this.detail = fields.detail
    this.instance = fields.instance
    this.problem402 = fields.problem402
    this.request_id = fields.request_id
    this.retry_after = fields.retry_after
    this.kind = fields.kind
  }
}

export interface ParseProblemInput {
  /** HTTP status, or `0` for a transport failure. */
  status: number
  /** Parsed `application/problem+json` body when available. */
  body?: unknown
  /** Value of the `X-Request-ID` response header, when present. */
  requestId?: string
  /** Raw `Retry-After` response header, when present. */
  retryAfter?: string
}

function asRecord(value: unknown): Record<string, unknown> | undefined {
  return typeof value === 'object' && value !== null
    ? (value as Record<string, unknown>)
    : undefined
}

function readString(
  source: Record<string, unknown> | undefined,
  key: string,
): string | undefined {
  const value = source?.[key]
  return typeof value === 'string' && value.length > 0 ? value : undefined
}

function readNumber(
  source: Record<string, unknown> | undefined,
  key: string,
): number | undefined {
  const value = source?.[key]
  return typeof value === 'number' && Number.isFinite(value) ? value : undefined
}

function readProblem402(
  body: Record<string, unknown> | undefined,
): Problem402 | undefined {
  const nested = asRecord(body?.problem402)
  const source = nested ?? body
  const balance = readNumber(source, 'balance')
  if (balance === undefined) return undefined

  const problem402: Problem402 = { balance }
  const required = readNumber(source, 'required')
  if (required !== undefined) problem402.required = required
  const currency = readString(source, 'currency')
  if (currency !== undefined) problem402.currency = currency
  const topupUrl = readString(source, 'topup_url')
  if (topupUrl !== undefined) problem402.topup_url = topupUrl

  return problem402
}

/**
 * `Retry-After` is emitted as delta-seconds by the gateway (429/503). Invalid
 * or HTTP-date values are ignored; callers then fall back to their own
 * documentation defaults rather than displaying garbage.
 */
function readRetryAfter(value: string | undefined): number | undefined {
  if (value === undefined) return undefined
  const seconds = Number(value)
  if (!Number.isFinite(seconds) || seconds < 0) return undefined
  return Math.ceil(seconds)
}

/**
 * Builds an `ApiError` from an HTTP status and an optional Problem body.
 *
 * `request_id` comes from the response header only (web-00 §13.3 C-2 froze the
 * header as the single source); the body field, if any, is ignored.
 */
export function parseProblem(input: ParseProblemInput): ApiError {
  const kind = kindForStatus(input.status)
  const body = asRecord(input.body)
  const fallbackTitle =
    input.status === 0 ? 'Network request failed' : `HTTP ${input.status}`

  return new ApiError({
    status: input.status,
    type: readString(body, 'type') ?? 'about:blank',
    title: readString(body, 'title') ?? fallbackTitle,
    detail: readString(body, 'detail'),
    instance: readString(body, 'instance'),
    problem402: kind === 'quota' ? readProblem402(body) : undefined,
    request_id: input.requestId,
    retry_after: readRetryAfter(input.retryAfter),
    kind,
  })
}
