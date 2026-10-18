import createClient, {
  type Client,
  type ClientOptions,
  type Middleware,
} from 'openapi-fetch'

import {
  CSRF_COOKIE,
  CSRF_HEADER,
  CSRF_REFRESH_PATH,
  REQUEST_ID_HEADER,
  WRITE_METHODS,
  getCsrfToken,
  isWriteMethod,
} from '@/lib/csrf'

import { ApiError, parseProblem } from './errors'
import type { paths } from './schema'

/**
 * Contract client (web-00 §5.2).
 *
 * `paths` comes from the generated `schema.d.ts` (Task 14), never a local
 * alias: it is the only compile-time link between the frozen OpenAPI contract
 * and this client, so endpoints that drift from the spec fail typecheck
 * instead of surfacing at runtime.
 */
export type ContractClient = Client<paths>

/**
 * The CSRF constants and cookie reader live in `lib/csrf` so non-client fetch
 * paths share the exact same names; re-exported here to keep the M0 import
 * path (`@/api/client`) stable.
 */
export { CSRF_COOKIE, CSRF_HEADER, REQUEST_ID_HEADER, CSRF_REFRESH_PATH, WRITE_METHODS, getCsrfToken }

async function readProblemBody(response: Response): Promise<unknown> {
  try {
    return await response.clone().json()
  } catch {
    return undefined
  }
}

/** Normalizes a non-2xx response, including transport-level metadata. */
export async function toApiError(response: Response): Promise<ApiError> {
  return parseProblem({
    status: response.status,
    body: await readProblemBody(response),
    requestId: response.headers.get(REQUEST_ID_HEADER) ?? undefined,
    retryAfter: response.headers.get('Retry-After') ?? undefined,
  })
}

/** Detects the single retryable auth failure: `403 csrf_failed`. */
export async function isCsrfFailure(response: Response): Promise<boolean> {
  if (response.status !== 403) return false
  const body = await readProblemBody(response)
  if (typeof body !== 'object' || body === null) return false
  const type = (body as Record<string, unknown>).type
  return (
    typeof type === 'string' &&
    (type === 'csrf_failed' ||
      type.endsWith('/csrf_failed') ||
      type.endsWith(':csrf_failed'))
  )
}

function isWrite(method: string): boolean {
  return isWriteMethod(method)
}

function newIdempotencyKey(): string {
  return globalThis.crypto.randomUUID()
}

/**
 * Unconsumed copies of outgoing requests, keyed by the exact `Request` object
 * openapi-fetch hands to `fetch()` and then to `onResponse()`. That object is
 * body-used after dispatch, so a replay must be built from this clone instead
 * (`new Request(usedRequest, …)` throws a TypeError and would be misreported
 * as `offline`).
 */
const retrySources = new WeakMap<Request, Request>()

/**
 * Frozen recovery procedure (security-01 §2.2, §3.5): re-issue the session
 * cookie with `GET /api/me` before replaying. The refresh is best-effort — the
 * replay decides the outcome and a second failure surfaces as `forbidden`.
 *
 * Exported so the feature-level raw-fetch client (channels `api.ts`) can reuse
 * the exact same recovery instead of re-implementing it.
 */
export async function refreshCsrfCookie(
  fetchImpl: typeof fetch = globalThis.fetch,
  baseUrl = '',
): Promise<void> {
  // Never call `fetchImpl(...)` as a method of an options object: browsers
  // brand-check native `fetch` and reject an object receiver with "Illegal
  // invocation". Passing the function as a value keeps the call unbound.
  const fallbackOrigin =
    typeof location !== 'undefined' ? location.origin : 'http://localhost'
  try {
    const url = new URL(CSRF_REFRESH_PATH, baseUrl || fallbackOrigin)
    await fetchImpl(
      new Request(url, {
        method: 'GET',
        credentials: 'same-origin',
        headers: { Accept: 'application/json' },
      }),
    )
  } catch {
    // Best effort only; the replay below still runs and normalizes its result.
  }
}

/**
 * Contract middleware:
 * - every write gets an `Idempotency-Key` (callers may supply their own to
 *   reuse it across retries of the same user intent, web-00 §5.6);
 * - POST/PUT/PATCH/DELETE get `X-CSRF-Token` read from `__Host-nkt_csrf`;
 * - a `403 csrf_failed` triggers the frozen recovery: `GET /api/me`, re-read
 *   the cookie, replay exactly once; a second failure stays `forbidden`;
 * - any other non-2xx response is thrown as a normalized `ApiError`;
 * - transport failures become `kind: 'offline'`.
 */
export const contractMiddleware: Middleware = {
  onRequest({ request }) {
    const headers = new Headers(request.headers)
    if (!headers.has('Accept')) headers.set('Accept', 'application/json')

    if (isWrite(request.method)) {
      if (!headers.has('Idempotency-Key')) {
        headers.set('Idempotency-Key', newIdempotencyKey())
      }
      const csrfToken = getCsrfToken()
      if (csrfToken !== undefined) headers.set(CSRF_HEADER, csrfToken)
    }

    const outgoing = new Request(request, { headers })
    if (isWrite(outgoing.method)) {
      retrySources.set(outgoing, outgoing.clone())
    }
    return outgoing
  },

  async onResponse({ request, response, options }) {
    if (response.ok) return response

    if (isWrite(request.method) && (await isCsrfFailure(response))) {
      const retrySource = retrySources.get(request)
      retrySources.delete(request)
      // Without an unconsumed copy a replay is impossible; surface the
      // original `forbidden` rather than misclassifying it as offline.
      if (retrySource === undefined) throw await toApiError(response)

      await refreshCsrfCookie(options.fetch ?? globalThis.fetch, options.baseUrl)

      const headers = new Headers(retrySource.headers)
      // Drop a stale token first: if the refresh did not produce a cookie the
      // replay must not keep resending the rejected value.
      headers.delete(CSRF_HEADER)
      const csrfToken = getCsrfToken()
      if (csrfToken !== undefined) headers.set(CSRF_HEADER, csrfToken)

      let retryRequest: Request
      try {
        retryRequest = new Request(retrySource, { headers })
      } catch {
        throw await toApiError(response)
      }

      let retried: Response
      try {
        // Capture the configured fetch as a bare function: calling
        // `options.fetch(...)` passes the frozen options object as `this`,
        // which native browser fetch rejects with "Illegal invocation".
        const doFetch = options.fetch ?? globalThis.fetch
        // A direct call (not `client.fetch`) means the middleware is not
        // re-entered, so this is structurally one retry.
        retried = await doFetch(retryRequest)
      } catch (error) {
        if (error instanceof TypeError) throw parseProblem({ status: 0 })
        throw error
      }

      if (!retried.ok) throw await toApiError(retried)
      return retried
    }

    throw await toApiError(response)
  },

  onError({ error }) {
    if (error instanceof ApiError) return error
    const offline =
      error instanceof TypeError ||
      (typeof navigator !== 'undefined' && navigator.onLine === false)
    if (offline) return parseProblem({ status: 0 })
    return error instanceof Error ? error : new Error(String(error))
  },
}

/**
 * Builds an independently middleware-wired client. Tests pass an absolute
 * `baseUrl`; production uses the same-origin default of the singleton below.
 */
export function createApiClient(options: ClientOptions = {}): ContractClient {
  const api = createClient<paths>({
    baseUrl: '',
    credentials: 'same-origin',
    // Late-bind the global fetch through a wrapper: openapi-fetch otherwise
    // captures whatever `fetch` exists at client-construction time, which in
    // tests is the unpatched Node fetch (MSW installs its interceptor later).
    // A wrapper keeps the same browser behaviour and lets test interception
    // apply to the application singleton.
    fetch: (input: Request) => globalThis.fetch(input),
    ...options,
  })
  api.use(contractMiddleware)
  return api
}

export const apiClient = createApiClient({
  // Absolute same-origin base: browsers resolve it identically to '', while
  // jsdom's Node-backed fetch (used by tests) rejects relative URLs.
  baseUrl: typeof location !== 'undefined' ? location.origin : '',
})
