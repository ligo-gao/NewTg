import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { waitFor } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'

import { ToastViewport, useToastStore } from '@/shared/ui/toast'
import { renderWithProviders } from '@/test-support/providers'

import { LanguageCard } from './LanguageCard'

const ORIGIN = 'http://localhost:3000'

const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => {
  server.resetHandlers()
  globalThis.localStorage?.clear()
  useToastStore.setState({ toasts: [] })
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
    server.use(
      http.get(`${ORIGIN}/api/self`, () => HttpResponse.json(selfSettings('en'))),
      http.put(`${ORIGIN}/api/self/setting`, async ({ request }) => {
        putBody = await request.json()
        return HttpResponse.json(selfSettings('fr'))
      }),
    )

    const { getByLabelText, getByText } = await renderWithProviders(
      <>
        <LanguageCard />
        <ToastViewport />
      </>,
      ['common', 'account'],
    )
    const user = userEvent.setup()

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

  it('keeps the chosen language when the server rejects the save', async () => {
    server.use(
      http.get(`${ORIGIN}/api/self`, () => HttpResponse.json(selfSettings('en'))),
      http.put(`${ORIGIN}/api/self/setting`, () =>
        HttpResponse.json(
          { type: 'conflict', title: 'Conflict' },
          { status: 409 },
        ),
      ),
    )

    const { getByRole, findByTestId } = await renderWithProviders(
      <>
        <LanguageCard />
        <ToastViewport />
      </>,
      ['common', 'account'],
    )
    const user = userEvent.setup()
    // 切换后卡片文案随界面语言变化，所以用控件角色而不是本地化标签定位。
    const select = (await waitFor(() => getByRole('combobox'))) as HTMLSelectElement

    await user.selectOptions(select, 'ja')

    // 写入失败不回滚：界面与下拉都保持 ja，pending 留给下次补写。
    await waitFor(() => {
      expect(document.documentElement.lang).toBe('ja')
      expect(select.value).toBe('ja')
    })
    expect(globalThis.localStorage.getItem('newtg.language.pending')).toBe('1')
    const viewport = await findByTestId('toast-viewport')
    expect(viewport.textContent).toContain('Language saved on this device')
  })
})
