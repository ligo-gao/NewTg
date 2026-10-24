import {
  createRootRouteWithContext,
  Outlet,
  redirect,
} from '@tanstack/react-router'

import { ErrorFallback } from '@/app/error-boundary'
import { useErrorPageTranslate } from '@/app/error-page-shell'
import { NotFoundPage } from '@/app/not-found-page'
import type { RouterContext } from '@/app/router'
import { fetchPublicStatus } from '@/lib/public-status'

const SETUP_PATH = '/setup'

function RootLayout() {
  // The router normally mounts inside `AppProviders`, but the root layout is
  // also reached by tests and by the global fallback path. `useErrorPageTranslate`
  // falls back to the module-level `translate` (common is the resident
  // baseline), so the skip link never renders a hardcoded English string.
  const t = useErrorPageTranslate()
  return (
    <>
      <a
        href="#main"
        className="sr-only focus:not-sr-only focus:fixed focus:left-3 focus:top-3 focus:z-50 focus:rounded-lg focus:bg-surface-card focus:px-3 focus:py-2 focus:text-body focus:text-ink-1 focus:shadow-[var(--focus-ring)]"
      >
        {t('common.nav.skip')}
      </a>
      <Outlet />
    </>
  )
}

/**
 * Root layer of the web-00 §9.3 guard chain: the initialization gate. When the
 * site is not initialized every route funnels to `/setup`; once initialized the
 * wizard redirects to sign-in (web-00 §9.4 row "未初始化" and the §9.9 note on
 * `/setup`). Session and role gates live on `_console` / `_console/admin`
 * because public routes must not block on `/api/me` (§9.2).
 *
 * A failed `GET /api/status` skips the gate (`fetchPublicStatus` returns null):
 * an outage must not brick the entire router — the pages that need status show
 * their own retryable error, and `/setup` remains protected server-side by the
 * setup endpoint's 409.
 *
 * `createRootRouteWithContext` is what makes `context.queryClient` visible to
 * every `beforeLoad`; the router supplies the shared client in `app/router`.
 */
export const Route = createRootRouteWithContext<RouterContext>()({
  beforeLoad: async ({ context, location }) => {
    const status = await fetchPublicStatus(context.queryClient)
    if (status === null) return
    if (!status.system_initialized && location.pathname !== SETUP_PATH) {
      throw redirect({ to: SETUP_PATH })
    }
    if (status.system_initialized && location.pathname === SETUP_PATH) {
      throw redirect({ to: '/sign-in' })
    }
  },
  component: RootLayout,
  errorComponent: ({ error, reset }) => (
    <ErrorFallback error={error} reset={reset} />
  ),
  // Same body as the `/404` route: render the shared page instead of keeping a
  // second inline copy that could drift from `_errors/404.tsx`.
  notFoundComponent: NotFoundPage,
})
