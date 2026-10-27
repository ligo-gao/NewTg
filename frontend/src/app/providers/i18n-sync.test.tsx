import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'

import { ME_QUERY_KEY, type Me } from '@/lib/session'
import { SELF_SETTINGS_QUERY_KEY } from '@/lib/self-settings'
import { LanguageCard } from '@/features/account/LanguageCard'
import { ToastViewport, useToastStore } from '@/shared/ui/toast'

import { I18nProvider, useI18n } from './i18n'

const ORIGIN = 'http://localhost:3000'

const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => {
  server.resetHandlers()
  globalThis.localStorage?.clear()
  // toast store 是模块级状态；不清理会让 info/success 断言串到下个用例。
  useToastStore.setState({ toasts: [] })
})
afterAll(() => server.close())

const ME: Me = {
  username: 'alice',
  email_verified: true,
  role: 'user',
  status: 'enabled',
  created_at: '2026-01-01T00:00:00Z',
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
    // 服务端偏好落地不是用户手势：不得伪造 explicit/pending 标记。
    expect(globalThis.localStorage.getItem('newtg.language.explicit')).toBeNull()
    expect(globalThis.localStorage.getItem('newtg.language.pending')).toBeNull()
  })

  it('a user gesture marks the choice explicit and pending', async () => {
    server.use(
      http.get(`${ORIGIN}/api/status`, () =>
        HttpResponse.json(status(['en', 'ja'])),
      ),
      http.get(`${ORIGIN}/api/self`, () => HttpResponse.json(self('en'))),
    )
    const { getByTestId, getByText } = renderSync()
    const user = userEvent.setup()
    await waitFor(() => expect(getByTestId('locale').textContent).toBe('en'))
    await user.click(getByText('switch-ja'))
    expect(globalThis.localStorage.getItem('newtg.language.explicit')).toBe('1')
    expect(globalThis.localStorage.getItem('newtg.language.pending')).toBe('1')
  })

  it('keeps the pending local locale and backfills it once', async () => {
    let putBody: unknown
    server.use(
      http.get(`${ORIGIN}/api/status`, () =>
        HttpResponse.json(status(['en', 'ja'])),
      ),
      http.get(`${ORIGIN}/api/self`, () => HttpResponse.json(self('en'))),
      http.put(`${ORIGIN}/api/self/setting`, async ({ request }) => {
        putBody = await request.json()
        return HttpResponse.json(self('ja', '2026-10-19T11:00:00Z'))
      }),
    )
    globalThis.localStorage.setItem('newtg.language', 'ja')
    globalThis.localStorage.setItem('newtg.language.pending', '1')
    const { getByTestId } = renderSync()
    await waitFor(() =>
      expect(putBody).toMatchObject({
        language: 'ja',
        updated_at: '2026-10-19T10:00:00Z',
      }),
    )
    expect(getByTestId('locale').textContent).toBe('ja')
    expect(globalThis.localStorage.getItem('newtg.language.pending')).toBeNull()
  })

  it('ignores pending when the local locale is not on sale (Review Focus 4)', async () => {
    server.use(
      http.get(`${ORIGIN}/api/status`, () =>
        HttpResponse.json(status(['en', 'fr'])),
      ),
      http.get(`${ORIGIN}/api/self`, () => HttpResponse.json(self('en'))),
    )
    globalThis.localStorage.setItem('newtg.language', 'ja')
    globalThis.localStorage.setItem('newtg.language.pending', '1')
    const { getByTestId } = renderSync()
    // ja 不在服务端语言集合里：不能被 pending 钉住，必须归一化到集合内。
    await waitFor(() => expect(getByTestId('locale').textContent).toBe('en'))
  })

  it('keeps pending when the backfill fails', async () => {
    server.use(
      http.get(`${ORIGIN}/api/status`, () =>
        HttpResponse.json(status(['en', 'ja'])),
      ),
      http.get(`${ORIGIN}/api/self`, () => HttpResponse.json(self('en'))),
      http.put(`${ORIGIN}/api/self/setting`, () =>
        HttpResponse.json(
          { type: 'conflict', title: 'Conflict' },
          { status: 409 },
        ),
      ),
    )
    globalThis.localStorage.setItem('newtg.language', 'ja')
    globalThis.localStorage.setItem('newtg.language.pending', '1')
    const { getByTestId } = renderSync()
    await waitFor(() => expect(getByTestId('locale').textContent).toBe('ja'))
    expect(globalThis.localStorage.getItem('newtg.language.pending')).toBe('1')
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

    const { getByTestId, getByRole, client } = renderSync()
    const user = userEvent.setup()

    await waitFor(() => {
      expect(selfRequested).toBe(true)
    })
    await user.click(getByRole('button', { name: 'switch-ja' }))
    await waitFor(() => {
      expect(getByTestId('locale').textContent).toBe('ja')
    })

    release()
    // 关键：必须等到响应真正写入缓存之后才能断言。仅同步检查当前语言会在
    // 响应微任务落地前通过，导致删掉手势守卫时用例仍可能假绿（评审复现）。
    await waitFor(() => {
      expect(client.getQueryData(SELF_SETTINGS_QUERY_KEY)).toBeDefined()
    })
    // 响应到达后不得回写 en/fr，也不得再覆盖 localStorage。
    expect(getByTestId('locale').textContent).toBe('ja')
    expect(globalThis.localStorage.getItem('newtg.language')).toBe('ja')
  })

  it('normalizes an out-of-set active locale to en when advertised (F5)', async () => {
    globalThis.localStorage.setItem('newtg.language', 'fr')
    server.use(
      http.get(`${ORIGIN}/api/status`, () =>
        HttpResponse.json(status(['ja', 'en'])),
      ),
    )

    const { getByTestId } = renderSync(<Probe />, false)

    await waitFor(() => {
      expect(getByTestId('available').textContent).toBe('ja,en')
      expect(getByTestId('locale').textContent).toBe('en')
    })
  })

  it('normalizes to the first advertised locale when en is absent (F5)', async () => {
    globalThis.localStorage.setItem('newtg.language', 'fr')
    server.use(
      http.get(`${ORIGIN}/api/status`, () =>
        HttpResponse.json(status(['ja', 'ru'])),
      ),
    )

    const { getByTestId } = renderSync(<Probe />, false)

    await waitFor(() => {
      expect(getByTestId('available').textContent).toBe('ja,ru')
      expect(getByTestId('locale').textContent).toBe('ja')
    })
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

  it('degrades to local-only with an info hint when self settings stay unavailable (F3)', async () => {
    let selfCalls = 0
    server.use(
      http.get(`${ORIGIN}/api/status`, () =>
        HttpResponse.json(status(['en', 'ja'])),
      ),
      http.get(`${ORIGIN}/api/self`, () => {
        selfCalls += 1
        return HttpResponse.json(
          { type: 'server_error', title: 'Unavailable' },
          { status: 503 },
        )
      }),
    )

    const { getByTestId, getByLabelText, findByTestId } = renderSync(
      <>
        <Probe />
        <LanguageCard />
        <ToastViewport />
      </>,
    )
    const user = userEvent.setup()

    await waitFor(() => {
      expect(selfCalls).toBeGreaterThanOrEqual(1)
    })
    await user.selectOptions(getByLabelText('Interface language'), 'ja')

    // 切换保留在本地；服务端偏好未保存，因此必须给出 local-only 提示，
    // 不能显示“Language updated”成功文案。
    await waitFor(() => {
      expect(getByTestId('locale').textContent).toBe('ja')
    })
    expect(globalThis.localStorage.getItem('newtg.language')).toBe('ja')
    const viewport = await findByTestId('toast-viewport')
    expect(viewport.textContent).toContain('Language changed on this device only')
    expect(viewport.textContent).not.toContain('Language updated')
    // 未就绪时会补读一次，但不会发出 PUT。
    expect(selfCalls).toBeGreaterThanOrEqual(2)
  })

  it('invalidates self settings after a failed save so a retry uses the fresh updated_at (F4)', async () => {
    let selfCalls = 0
    const putBodies: Array<Record<string, unknown>> = []
    server.use(
      http.get(`${ORIGIN}/api/status`, () =>
        HttpResponse.json(status(['en', 'ja'])),
      ),
      http.get(`${ORIGIN}/api/self`, () => {
        selfCalls += 1
        // 第一次读取给出旧令牌；失效后的重新读取给出新令牌。
        return HttpResponse.json(
          self('en', selfCalls === 1 ? '2026-10-19T10:00:00Z' : '2026-10-19T12:00:00Z'),
        )
      }),
      http.put(`${ORIGIN}/api/self/setting`, async ({ request }) => {
        const body = (await request.json()) as Record<string, unknown>
        putBodies.push(body)
        if (body.updated_at === '2026-10-19T10:00:00Z') {
          return HttpResponse.json(
            { type: 'conflict', title: 'Conflict' },
            { status: 409 },
          )
        }
        return HttpResponse.json(self('ja', '2026-10-19T13:00:00Z'))
      }),
    )

    const { getByTestId, getByLabelText } = renderSync(
      <>
        <Probe />
        <LanguageCard />
      </>,
    )
    const user = userEvent.setup()

    await waitFor(() => {
      expect(getByTestId('locale').textContent).toBe('en')
    })

    // 第一次保存撞 409：回退 en，并触发个人设置失效/重读。
    await user.selectOptions(getByLabelText('Interface language'), 'ja')
    await waitFor(() => {
      expect(getByTestId('locale').textContent).toBe('en')
      expect(selfCalls).toBeGreaterThanOrEqual(2)
    })

    // 重试必须使用刷新后的 updated_at；旧令牌会让服务端持续 409。
    await waitFor(() => {
      expect(
        (getByLabelText('Interface language') as HTMLSelectElement).disabled,
      ).toBe(false)
    })
    await user.selectOptions(getByLabelText('Interface language'), 'ja')

    await waitFor(() => {
      expect(getByTestId('locale').textContent).toBe('ja')
    })
    expect(putBodies).toHaveLength(2)
    expect(putBodies[1]?.updated_at).toBe('2026-10-19T12:00:00Z')
  })
})
