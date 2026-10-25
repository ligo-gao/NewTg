import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, type RenderResult } from '@testing-library/react'
import type { ReactNode } from 'react'

import { I18nProvider, loadDictionary, type DictionaryModule } from '@/app/providers/i18n'

/**
 * Renders a feature component with the same providers the app mounts, using a
 * fresh QueryClient per test (query caches are module state otherwise) and
 * pre-loading the dictionaries so assertions can use final copy immediately.
 *
 * The locale is pinned to `en`: the product default is `zh-CN`, but these
 * suites assert English copy. Pin it explicitly instead of relying on stored
 * state, because several suites clear localStorage in their own `beforeEach`.
 */
export async function renderWithProviders(
  ui: ReactNode,
  modules: readonly DictionaryModule[] = ['common'],
): Promise<RenderResult> {
  await Promise.all(modules.map((module) => loadDictionary('en', module)))

  const client = new QueryClient({
    defaultOptions: {
      queries: { retry: false, refetchOnWindowFocus: false, staleTime: 0 },
      mutations: { retry: 0 },
    },
  })

  return render(
    <QueryClientProvider client={client}>
      <I18nProvider locale="en" modules={modules}>
        {ui}
      </I18nProvider>
    </QueryClientProvider>,
  )
}
