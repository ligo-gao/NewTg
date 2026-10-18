import { createRootRoute, Outlet } from '@tanstack/react-router'

import { ErrorFallback } from '@/app/error-boundary'
import { NotFoundPage } from '@/app/not-found-page'

function RootLayout() {
  return (
    <>
      <a
        href="#main"
        className="sr-only focus:not-sr-only focus:fixed focus:left-3 focus:top-3 focus:z-50 focus:rounded-lg focus:bg-surface-card focus:px-3 focus:py-2 focus:text-sm focus:text-ink-1 focus:shadow-[var(--focus-ring)]"
      >
        Skip to content
      </a>
      <Outlet />
    </>
  )
}

export const Route = createRootRoute({
  component: RootLayout,
  errorComponent: ({ error, reset }) => (
    <ErrorFallback error={error} reset={reset} />
  ),
  // Same body as the `/404` route: render the shared page instead of keeping a
  // second inline copy that could drift from `_errors/404.tsx`.
  notFoundComponent: NotFoundPage,
})
