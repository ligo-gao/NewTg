import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { RouterProvider } from '@tanstack/react-router'

import { ErrorBoundary } from '@/app/error-boundary'
import { AppProviders } from '@/app/providers/app-providers'
import { router } from '@/app/router'

import '@/styles/tokens.css'
import '@/styles/tailwind.css'
// Shared public header/footer (packages/public-shell). This is the only
// route-independent CSS above the layouts: page-level styles (home.css,
// auth-hall.css) and the self-hosted shell fonts are imported by the layout
// that actually renders them, so the auth entry no longer downloads the
// homepage bundle and the console downloads neither.
import '@newtg/public-shell/styles.css'

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
