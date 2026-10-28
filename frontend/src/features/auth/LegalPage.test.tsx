import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import { within } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { LegalPage } from './LegalPage'

const ORIGIN = 'http://localhost:3000'

const server = setupServer(
  http.get(`${ORIGIN}/api/status`, () =>
    HttpResponse.json({
      languages: ['en'],
      default_language: 'en',
      site: { title: 'Acme Gateway' },
    }),
  ),
  http.get(`${ORIGIN}/api/me`, () => HttpResponse.json({}, { status: 401 })),
)

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

describe('LegalPage', () => {
  it('renders the document and the shared public footer', async () => {
    const result = await renderWithProviders(<LegalPage doc="terms" />, [
      'common',
      'auth',
    ])

    const footer = await result.findByRole('contentinfo')
    expect(result.getAllByRole('contentinfo')).toHaveLength(1)
    const openai = await within(footer).findByRole('link', {
      name: 'OpenAI (Opens in a new tab)',
    })
    expect(openai.getAttribute('href')).toBe('https://openai.com')
    expect(
      within(footer)
        .getByRole('link', { name: 'Terms of service' })
        .getAttribute('href'),
    ).toBe('/legal/terms')
  })

  it('uses the shared auth frame so a short document keeps the footer at the viewport bottom', async () => {
    const result = await renderWithProviders(<LegalPage doc="privacy" />, [
      'common',
      'auth',
    ])

    const footer = await result.findByRole('contentinfo')
    const frame = result.container.querySelector('.auth-hall-frame')
    // 内层不再自己 min-h-screen：高度只由 frame 一份来源分配，短文档的
    // 页脚才会被内容区的 flex:1 推到视口底部，而不是停在屏幕中间。
    expect(frame).not.toBeNull()
    expect(result.container.querySelector('.min-h-screen')).toBeNull()
    expect(frame?.querySelector('main#main')).not.toBeNull()
    // 页脚是 frame 的最后一个直接子节点，背景也由 frame 覆盖到底。
    expect(frame?.lastElementChild).toBe(footer)
  })
})
