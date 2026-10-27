import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { RouterProvider } from '@tanstack/react-router'

import { ErrorBoundary } from '@/app/error-boundary'
import { AppProviders } from '@/app/providers/app-providers'
import { router } from '@/app/router'

import '@/styles/tokens.css'
import '@/styles/tailwind.css'
// Shared public header/footer (packages/public-shell). Loaded before home.css
// so a page-level rule can still win when it needs to.
import '@newtg/public-shell/styles.css'
import '@/styles/home.css'
import '@/styles/auth-hall.css'

const rootElement = document.getElementById('root')

if (!rootElement) {
  throw new Error('Root element #root was not found in index.html')
}

createRoot(rootElement).render(
  <StrictMode>
    {/*
      The boundary sits outside AppProviders so a provider-level render failure
      (Query or i18n) also reaches the shared fallback. Route-level errors are
      caught one level lower by the root route's `errorComponent`, which renders
      the same ErrorFallback.
    */}
    <ErrorBoundary>
      <AppProviders>
        <RouterProvider router={router} />
      </AppProviders>
    </ErrorBoundary>
  </StrictMode>,
)
