import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { waitFor } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { LanguageCard } from './LanguageCard'

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

describe('LanguageCard (F-01-19)', () => {
  it('lists the advertised languages and persists a switch to the server', async () => {
    let putBody: unknown
    let getDone = false
    server.use(
      http.get(`${ORIGIN}/api/self`, () => {
        getDone = true
        return HttpResponse.json(selfSettings('en'))
      }),
      http.put(`${ORIGIN}/api/self/setting`, async ({ request }) => {
        putBody = await request.json()
        return HttpResponse.json(selfSettings('fr'))
      }),
    )

    const { getByLabelText, getByText } = await renderWithProviders(
      <LanguageCard />,
      ['common', 'account'],
    )
    const user = userEvent.setup()
    // 等个人设置加载完成，否则保存路径会因缺少 updated_at 而降级为仅本地。
    await waitFor(() => {
      expect(getDone).toBe(true)
    })

    // 语言集合来自运行时（status ∩ 随包词典），下拉顺序即 canonical 顺序。
    const select = (await waitFor(() => {
      const element = getByLabelText('Interface language')
      expect(element.tagName).toBe('SELECT')
      return element
    })) as HTMLSelectElement
    expect(select.options).toHaveLength(7)

    await user.selectOptions(select, 'fr')

    await waitFor(() => {
      expect(putBody).toMatchObject({
        language: 'fr',
        updated_at: '2026-10-19T10:00:00Z',
      })
    })
    // 切换即时生效：<html lang> 与选中项同步更新；本地记忆写入 localStorage。
    await waitFor(() => {
      expect(document.documentElement.lang).toBe('fr')
      expect(
        (getByLabelText(/interface/i) as HTMLSelectElement).value,
      ).toBe('fr')
    })
    expect(globalThis.localStorage.getItem('newtg.language')).toBe('fr')
    await waitFor(() => {
      expect(getByText('Langue')).toBeTruthy()
    })
  })

  it('falls back to the previous language when the server rejects the save', async () => {
    let getDone = false
    server.use(
      http.get(`${ORIGIN}/api/self`, () => {
        getDone = true
        return HttpResponse.json(selfSettings('en'))
      }),
      http.put(`${ORIGIN}/api/self/setting`, () =>
        HttpResponse.json(
          { type: 'conflict', title: 'Conflict' },
          { status: 409 },
        ),
      ),
    )

    const { getByLabelText } = await renderWithProviders(<LanguageCard />, [
      'common',
      'account',
    ])
    const user = userEvent.setup()
    await waitFor(() => {
      expect(getDone).toBe(true)
    })
    const select = (await waitFor(() =>
      getByLabelText('Interface language'),
    )) as HTMLSelectElement

    await user.selectOptions(select, 'ja')

    // 写入失败后回退原语言（modules-01 F-01-19 异常流程）。
    await waitFor(() => {
      expect(document.documentElement.lang).toBe('en')
      expect(
        (getByLabelText('Interface language') as HTMLSelectElement).value,
      ).toBe('en')
    })
  })
})
