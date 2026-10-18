/**
 * CSRF plumbing for non-`apiClient` fetch paths (feat/web-00 §5.2,
 * security-01 §2.2/§3.5).
 *
 * The contract client in `api/client.ts` already injects `X-CSRF-Token` and
 * owns the frozen `403 csrf_failed` recovery. This module exists for the few
 * call sites that cannot go through openapi-fetch (raw downloads, SSE readers,
 * the email-code sender) so they share the exact same cookie name, header and
 * replay semantics instead of re-implementing them.
 */

export const CSRF_COOKIE = '__Host-nkt_csrf'
export const CSRF_HEADER = 'X-CSRF-Token'
export const REQUEST_ID_HEADER = 'X-Request-ID'
export const CSRF_REFRESH_PATH = '/api/me'

/** Methods that mutate server state and therefore carry the CSRF header. */
export const WRITE_METHODS: ReadonlySet<string> = new Set([
  'POST',
  'PUT',
  'PATCH',
  'DELETE',
])

export function isWriteMethod(method: string): boolean {
  return WRITE_METHODS.has(method.toUpperCase())
}

/** Reads the non-HttpOnly CSRF cookie. Returns undefined outside the browser. */
export function getCsrfToken(): string | undefined {
  if (typeof document === 'undefined') return undefined
  const prefix = `${CSRF_COOKIE}=`
  const entry = document.cookie
    .split(';')
    .map((part) => part.trim())
    .find((part) => part.startsWith(prefix))
  if (entry === undefined) return undefined
  const raw = entry.slice(prefix.length)
  try {
    return decodeURIComponent(raw)
  } catch {
    return raw
  }
}

function currentOrigin(): string {
  // Node's fetch (used by jsdom tests) rejects relative URLs; resolving here
  // also keeps the browser behaviour explicit rather than relying on fetch's
  // implicit base URL resolution.
  return typeof location !== 'undefined' ? location.origin : 'http://localhost'
}

function absoluteUrl(input: RequestInfo | URL): string {
  if (typeof input === 'string') return new URL(input, currentOrigin()).toString()
  if (input instanceof URL) return input.toString()
  return input.url
}

async function isCsrfFailure(response: Response): Promise<boolean> {
  if (response.status !== 403) return false
  try {
    const body: unknown = await response.clone().json()
    if (typeof body !== 'object' || body === null) return false
    const type = (body as Record<string, unknown>).type
    return (
      typeof type === 'string' &&
      (type === 'csrf_failed' ||
        type.endsWith('/csrf_failed') ||
        type.endsWith(':csrf_failed'))
    )
  } catch {
    return false
  }
}

/** Best-effort cookie re-issue; the replay below decides the outcome. */
async function refreshCsrfCookie(): Promise<void> {
  try {
    await fetch(new URL(CSRF_REFRESH_PATH, currentOrigin()), {
      method: 'GET',
      credentials: 'same-origin',
      headers: { Accept: 'application/json' },
    })
  } catch {
    // Offline or transient failure: replay anyway and surface its result.
  }
}

async function send(template: Request): Promise<Response> {
  const headers = new Headers(template.headers)
  if (isWriteMethod(template.method)) {
    // Always replace a stale value: a caller-supplied token must not survive a
    // rotation performed by another tab.
    headers.delete(CSRF_HEADER)
    const token = getCsrfToken()
    if (token !== undefined) headers.set(CSRF_HEADER, token)
  }
  return fetch(new Request(template, { headers }))
}

/**
 * `fetch` with CSRF injection and the frozen single retry: a 401 (session
 * rotated) or `403 csrf_failed` triggers `GET /api/me`, then the request is
 * replayed once with a freshly read cookie. Any second failure is returned to
 * the caller unchanged — never looped.
 */
export async function csrfFetch(
  input: RequestInfo | URL,
  init: RequestInit = {},
): Promise<Response> {
  // The template is never dispatched: each attempt clones it so a body is
  // available again for the replay (`Request` bodies are single-use).
  const template = new Request(absoluteUrl(input), init)

  const first = await send(template.clone())
  const retryable = first.status === 401 || (await isCsrfFailure(first))
  if (!retryable) return first

  await refreshCsrfCookie()
  return send(template.clone())
}
