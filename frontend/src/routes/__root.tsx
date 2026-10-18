import { createRootRoute, Link, Outlet } from '@tanstack/react-router'

import { ErrorFallback } from '@/app/error-boundary'

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

function NotFound() {
  return (
    <main
      id="main"
      className="flex min-h-screen flex-col items-center justify-center gap-3 bg-surface-page p-6 text-center text-ink-1"
    >
      <h1 className="text-3xl font-bold">404</h1>
      <p className="text-sm text-ink-2">Page not found</p>
      <Link
        to="/"
        className="text-sm font-medium text-brand underline-offset-2 hover:text-brand-hover hover:underline"
      >
        Back to home
      </Link>
    </main>
  )
}

export const Route = createRootRoute({
  component: RootLayout,
  errorComponent: ({ error, reset }) => (
    <ErrorFallback error={error} reset={reset} />
  ),
  notFoundComponent: NotFound,
})
