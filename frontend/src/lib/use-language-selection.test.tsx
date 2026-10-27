import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from 'vitest'

import { I18nProvider, loadDictionary } from '@/app/providers/i18n'
import { useI18n, type Locale } from '@/shared/lib/i18n-context'
import { LanguageSwitch } from '@/shared/ui/language-switch'
import { ToastViewport, useToastStore } from '@/shared/ui/toast'
import { renderWithProviders } from '@/test-support/providers'

import { useLanguageSelection } from './use-language-selection'

const ORIGIN = 'http://localhost:3000'

const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => {
  server.resetHandlers()
  globalThis.localStorage?.clear()
  useToastStore.setState({ toasts: [] })
  vi.restoreAllMocks()
})
afterAll(() => server.close())

function selfSettings(language: string) {
  return {
    language,
    sidebar_modules: null,
    record_ip_log: false,
    updated_at: '2026-10-19T10:00:00Z',
  }
}

/** Mirrors how `AppTopbar` wires the hook into `LanguageSwitch`. */
function LanguageHarness({ signedIn }: { signedIn: boolean }) {
  const { t } = useI18n()
  const { onChange } = useLanguageSelection(signedIn)
  return (
    <>
      <LanguageSwitch
        label={t('common.language.label')}
        onChange={onChange}
      />
      <ToastViewport />
    </>
  )
}

function renderHarness(signedIn: boolean) {
  return renderWithProviders(<LanguageHarness signedIn={signedIn} />, [
    'common',
  ])
}

/**
 * Saves without touching the picker, so the locale stays put while the toast is
 * produced. That keeps the expected copy unambiguous: the hook translates with
 * the locale active when the gesture started (the `LanguageSwitch` handler runs
 * before React re-renders for the new locale).
 */
function SaveProbe() {
  const { onChange } = useLanguageSelection(true)
  return (
    <>
      <button type="button" onClick={() => void onChange?.('ja')}>
        save-language
      </button>
      <ToastViewport />
    </>
  )
}

async function renderSaveProbe(locale: Locale) {
  await loadDictionary(locale, 'common')
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  return render(
    <QueryClientProvider client={client}>
      <I18nProvider locale={locale} modules={['common']}>
        <SaveProbe />
      </I18nProvider>
    </QueryClientProvider>,
  )
}

describe('useLanguageSelection', () => {
  it('persists the chosen language to the profile for a signed-in user', async () => {
    let putBody: unknown
    server.use(
      http.get(`${ORIGIN}/api/self`, () => HttpResponse.json(selfSettings('en'))),
      http.put(`${ORIGIN}/api/self/setting`, async ({ request }) => {
        putBody = await request.json()
        return HttpResponse.json(selfSettings('fr'))
      }),
    )

    const { getByLabelText } = await renderHarness(true)
    const select = getByLabelText('Interface language') as HTMLSelectElement
    const user = userEvent.setup()

    await user.selectOptions(select, 'fr')

    await waitFor(() => {
      expect(putBody).toMatchObject({
        language: 'fr',
        updated_at: '2026-10-19T10:00:00Z',
      })
    })
    await waitFor(() => {
      expect(document.documentElement.lang).toBe('fr')
    })
  })

  it('shows the localized success toast instead of the raw key', async () => {
    server.use(
      http.get(`${ORIGIN}/api/self`, () => HttpResponse.json(selfSettings('en'))),
      http.put(`${ORIGIN}/api/self/setting`, () =>
        HttpResponse.json(selfSettings('ja')),
      ),
    )

    const { getByText, findByTestId } = await renderSaveProbe('fr')
    const user = userEvent.setup()

    await user.click(getByText('save-language'))

    const viewport = await findByTestId('toast-viewport')
    // `common.language.updated` must exist in every bundle: a missing key makes
    // `translate` return the key itself, which would leak into the UI.
    expect(viewport.textContent).toContain('Langue mise à jour')
    expect(viewport.textContent).not.toContain('common.language.updated')
  })

  it('keeps the chosen language and pending flag when the server rejects the save', async () => {
    server.use(
      http.get(`${ORIGIN}/api/self`, () => HttpResponse.json(selfSettings('en'))),
      http.put(`${ORIGIN}/api/self/setting`, () =>
        HttpResponse.json(
          { type: 'conflict', title: 'Conflict' },
          { status: 409 },
        ),
      ),
    )

    const { getByLabelText, findByTestId } = await renderHarness(true)
    const select = getByLabelText('Interface language') as HTMLSelectElement
    const user = userEvent.setup()

    await user.selectOptions(select, 'ja')

    await waitFor(() => {
      expect(document.documentElement.lang).toBe('ja')
    })
    expect(globalThis.localStorage.getItem('newtg.language.pending')).toBe('1')
    const viewport = await findByTestId('toast-viewport')
    expect(viewport.textContent).toContain('Language saved on this device')
  })

  it('keeps the switch local for a guest and never calls the profile API', async () => {
    let profileCalls = 0
    server.use(
      http.get(`${ORIGIN}/api/self`, () => {
        profileCalls += 1
        return HttpResponse.json(selfSettings('en'))
      }),
      http.put(`${ORIGIN}/api/self/setting`, () => {
        profileCalls += 1
        return HttpResponse.json(selfSettings('fr'))
      }),
    )

    const { getByLabelText } = await renderHarness(false)
    const select = getByLabelText('Interface language') as HTMLSelectElement
    const user = userEvent.setup()

    await user.selectOptions(select, 'fr')

    expect(document.documentElement.lang).toBe('fr')
    expect(globalThis.localStorage.getItem('newtg.language')).toBe('fr')
    expect(profileCalls).toBe(0)
  })

  it('keeps the local switch when the profile cannot be read', async () => {
    // The observer is subscribed with `enabled: false`; a signed-in session
    // whose cache never filled must still keep the local switch usable.
    server.use(
      http.get(`${ORIGIN}/api/self`, () => new HttpResponse(null, { status: 401 })),
    )

    const { getByLabelText } = await renderHarness(true)
    const select = getByLabelText('Interface language') as HTMLSelectElement
    const user = userEvent.setup()

    await user.selectOptions(select, 'fr')

    expect(document.documentElement.lang).toBe('fr')
  })

  it('keeps the switch working when storage is blocked (Review Focus 1)', async () => {
    server.use(
      http.get(`${ORIGIN}/api/self`, () => HttpResponse.json(selfSettings('en'))),
      http.put(`${ORIGIN}/api/self/setting`, () =>
        HttpResponse.json(selfSettings('fr')),
      ),
    )
    const spy = vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new Error('blocked')
    })

    try {
      const { getByLabelText } = await renderHarness(true)
      const select = getByLabelText('Interface language') as HTMLSelectElement
      const user = userEvent.setup()

      await user.selectOptions(select, 'fr')

      expect(document.documentElement.lang).toBe('fr')
    } finally {
      spy.mockRestore()
    }
  })
})
