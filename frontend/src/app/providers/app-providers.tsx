import type { ReactNode } from 'react'

import { I18nProvider } from './i18n'
import { QueryProvider } from './query'
import { ToastViewport } from '@/shared/ui/toast'

export interface AppProvidersProps {
  children: ReactNode
}

/**
 * Application-wide providers.
 *
 * The Query client owns all server state (web-00 §5); i18n is a minimal
 * English-only runtime for M0 (web-00 §7). MVP is permanently light, so there
 * is no theme provider or `dark:` variant yet. The toast viewport is mounted
 * once here so any layer can surface global feedback (modules-00 §4.8).
 */
export function AppProviders({ children }: AppProvidersProps) {
  return (
    <QueryProvider>
      <I18nProvider>
        {children}
        <ToastViewport />
      </I18nProvider>
    </QueryProvider>
  )
}
