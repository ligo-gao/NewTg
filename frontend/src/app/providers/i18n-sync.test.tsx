import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'

import { ME_QUERY_KEY, type Me } from '@/lib/session'
import { SELF_SETTINGS_QUERY_KEY } from '@/lib/self-settings'
import { LanguageCard } from '@/features/account/LanguageCard'

import { I18nProvider, useI18n } from './i18n'

const ORIGIN = 'http://localhost:3000'

const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => {
  server.resetHandlers()
  globalThis.localStorage?.clear()
})
afterAll(() => server.close())

const ME: Me = {
  username: 'alice',
  role: 'user',
  status: 'enabled',
  permissions: [],
}

function status(languages: string[]) {
  return { languages, default_language: languages[0] ?? 'en' }
}

function self(language: string, updatedAt = '2026-10-19T10:00:00Z') {
  return {
    language,
    sidebar_modules: null,
    record_ip_log: false,
    updated_at: updatedAt,
  }
}

function Probe() {
  const { locale, availableLocales, setLocale } = useI18n()
  return (
    <div>
      <span data-testid="locale">{locale}</span>
      <span data-testid="available">{(availableLocales ?? []).join(',')}</span>
      <button
        type="button"
        onClick={() => {
          setLocale?.('ja')
        }}
      >
        switch-ja
      </button>
    </div>
  )
}

function renderSync(ui = <Probe />, seedMe = true) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  if (seedMe) client.setQueryData(ME_QUERY_KEY, ME)
  const result = render(
    <QueryClientProvider client={client}>
      <I18nProvider syncPreference modules={['common', 'account']}>
        {ui}
      </I18nProvider>
    </QueryClientProvider>,
  )
  return { ...result, client }
}

describe('LocalePreferenceSync', () => {
  it('publishes the status language set in server order (F5/F7)', async () => {
    server.use(
      http.get(`${ORIGIN}/api/status`, () =>
        HttpResponse.json(status(['ja', 'en', 'zh-TW'])),
      ),
      http.get(`${ORIGIN}/api/self`, () => HttpResponse.json(self('en'))),
    )

    const { getByTestId } = renderSync()

    await waitFor(() => {
      expect(getByTestId('available').textContent).toBe('ja,en,zh-TW')
      expect(getByTestId('locale').textContent).toBe('en')
    })
  })

  it('applies the server preference when no gesture occurred yet', async () => {
    server.use(
      http.get(`${ORIGIN}/api/status`, () =>
        HttpResponse.json(status(['en', 'fr'])),
      ),
      http.get(`${ORIGIN}/api/self`, () => HttpResponse.json(self('fr'))),
    )

    const { getByTestId } = renderSync()

    await waitFor(() => {
      expect(getByTestId('locale').textContent).toBe('fr')
    })
    expect(globalThis.localStorage.getItem('newtg.language')).toBe('fr')
  })

  it('never applies the initial preference over a user gesture (F1)', async () => {
    let release!: () => void
    const gate = new Promise<void>((resolve) => {
      release = resolve
    })
    let selfRequested = false
    server.use(
      http.get(`${ORIGIN}/api/status`, () =>
        HttpResponse.json(status(['en', 'fr', 'ja'])),
      ),
      http.get(`${ORIGIN}/api/self`, async () => {
        selfRequested = true
        await gate
        return HttpResponse.json(self('fr'))
      }),
    )

    const { getByTestId, getByRole } = renderSync()
    const user = userEvent.setup()

    await waitFor(() => {
      expect(selfRequested).toBe(true)
    })
    await user.click(getByRole('button', { name: 'switch-ja' }))
    await waitFor(() => {
      expect(getByTestId('locale').textContent).toBe('ja')
    })

    release()
    // 响应到达后不得回写 en/fr，也不得再覆盖 localStorage。
    await waitFor(() => {
      expect(getByTestId('locale').textContent).toBe('ja')
    })
    expect(globalThis.localStorage.getItem('newtg.language')).toBe('ja')
  })

  it('does not request /api/self for anonymous visitors (F7)', async () => {
    let selfCalls = 0
    server.use(
      http.get(`${ORIGIN}/api/status`, () =>
        HttpResponse.json(status(['en', 'fr'])),
      ),
      http.get(`${ORIGIN}/api/self`, () => {
        selfCalls += 1
        return HttpResponse.json(self('fr'))
      }),
    )

    const { getByTestId } = renderSync(<Probe />, false)

    await waitFor(() => {
      expect(getByTestId('available').textContent).toBe('en,fr')
    })
    expect(selfCalls).toBe(0)
    expect(getByTestId('locale').textContent).toBe('en')
  })

  it('writes the saved preference back into the shared cache (F7)', async () => {
    server.use(
      http.get(`${ORIGIN}/api/status`, () =>
        HttpResponse.json(status(['en', 'fr'])),
      ),
      http.get(`${ORIGIN}/api/self`, () => HttpResponse.json(self('en'))),
      http.put(`${ORIGIN}/api/self/setting`, () =>
        HttpResponse.json(self('fr', '2026-10-19T11:00:00Z')),
      ),
    )

    const { getByTestId, getByLabelText, client } = renderSync(
      <>
        <Probe />
        <LanguageCard />
      </>,
    )
    const user = userEvent.setup()

    await waitFor(() => {
      expect(getByTestId('locale').textContent).toBe('en')
    })
    await user.selectOptions(
      getByLabelText('Interface language'),
      'fr',
    )

    await waitFor(() => {
      expect(getByTestId('locale').textContent).toBe('fr')
    })
    // 卡片把 PUT 响应写回同一缓存，同步器因此不会用旧值回滚。
    expect(
      client.getQueryData<{ language: string }>(SELF_SETTINGS_QUERY_KEY)
        ?.language,
    ).toBe('fr')
    expect(
      (getByLabelText(/interface/i) as HTMLSelectElement).value,
    ).toBe('fr')
  })
})
