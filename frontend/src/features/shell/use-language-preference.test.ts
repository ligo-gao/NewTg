import { createElement } from 'react'

import { waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'

import { useI18n } from '@/shared/lib/i18n-context'
import { LanguageSwitch } from '@/shared/ui/language-switch'
import { renderWithProviders } from '@/test-support/providers'

import { useLanguagePreference } from './use-language-preference'

const ORIGIN = 'http://localhost:3000'

const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => {
  server.resetHandlers()
  globalThis.localStorage?.clear()
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
  const { onChange } = useLanguagePreference(signedIn)
  return createElement(LanguageSwitch, {
    label: t('common.language.label'),
    onChange,
  })
}

function renderHarness(signedIn: boolean) {
  return renderWithProviders(
    createElement(LanguageHarness, { signedIn }),
    ['common'],
  )
}

describe('useLanguagePreference', () => {
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

  it('restores the previous language when the server rejects the save', async () => {
    server.use(
      http.get(`${ORIGIN}/api/self`, () => HttpResponse.json(selfSettings('en'))),
      http.put(`${ORIGIN}/api/self/setting`, () =>
        HttpResponse.json(
          { type: 'conflict', title: 'Conflict' },
          { status: 409 },
        ),
      ),
    )

    const { getByLabelText } = await renderHarness(true)
    const select = getByLabelText('Interface language') as HTMLSelectElement
    const user = userEvent.setup()

    await user.selectOptions(select, 'ja')

    await waitFor(() => {
      expect(document.documentElement.lang).toBe('en')
      expect(
        (getByLabelText('Interface language') as HTMLSelectElement).value,
      ).toBe('en')
    })
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
})
