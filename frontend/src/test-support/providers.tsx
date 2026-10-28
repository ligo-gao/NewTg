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
/**
 * 需要预置缓存或观察查询状态的用例可以传入自己的 client（例如验证
 * 「有缓存 + 刷新失败」仍然可用）；不传时每个测试仍拿到全新的缓存。
 */
export interface RenderWithProvidersOptions {
  client?: QueryClient
}

export async function renderWithProviders(
  ui: ReactNode,
  modules: readonly DictionaryModule[] = ['common'],
  options: RenderWithProvidersOptions = {},
): Promise<RenderResult & { client: QueryClient }> {
  await Promise.all(modules.map((module) => loadDictionary('en', module)))

  const client =
    options.client ??
    new QueryClient({
      defaultOptions: {
        queries: { retry: false, refetchOnWindowFocus: false, staleTime: 0 },
        mutations: { retry: 0 },
      },
    })

  const result = render(
    <QueryClientProvider client={client}>
      <I18nProvider locale="en" modules={modules}>
        {ui}
      </I18nProvider>
    </QueryClientProvider>,
  )
  return Object.assign(result, { client })
}
