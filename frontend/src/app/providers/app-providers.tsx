import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { useState, type ReactNode } from 'react'

export interface AppProvidersProps {
  children: ReactNode
}

/**
 * Application-wide providers.
 *
 * The Query client owns all server state (web-00 §5). i18n and theme providers
 * are added by their own tasks; MVP is permanently light, so there is no theme
 * switching surface or `dark:` variant here.
 */
export function AppProviders({ children }: AppProvidersProps) {
  const [queryClient] = useState(
    () =>
      new QueryClient({
        defaultOptions: {
          queries: {
            retry: 1,
            refetchOnWindowFocus: false,
            staleTime: 30_000,
          },
        },
      }),
  )

  return (
    <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
  )
}
