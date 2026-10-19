import { cleanup, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it } from 'vitest'

import { I18nProvider, useI18n } from './i18n'

afterEach(() => {
  cleanup()
  globalThis.localStorage?.clear()
})

function Probe() {
  const { t, locale, setLocale } = useI18n()
  return (
    <div>
      <p>{t('common.app.title')}</p>
      <p data-testid="tagline">{t('public.home.tagline')}</p>
      <p data-testid="locale">{locale}</p>
      <button
        type="button"
        onClick={() => {
          setLocale?.('ja')
        }}
      >
        switch
      </button>
    </div>
  )
}

describe('I18nProvider', () => {
  it('loads the app-wide common domain and re-renders when it lands', async () => {
    render(
      <I18nProvider>
        <Probe />
      </I18nProvider>,
    )

    // 加载器是异步的，首次绘制回退为键名；provider 的 revision bump 必须在
    // chunk 到达后替换成真实文案。
    expect(screen.getAllByText('common.app.title').length).toBeGreaterThan(0)
    await waitFor(() => {
      expect(screen.getByText('NewTG')).toBeTruthy()
    })
  })

  it('sets <html lang> from the active locale', () => {
    render(
      <I18nProvider locale="en">
        <Probe />
      </I18nProvider>,
    )

    expect(document.documentElement.lang).toBe('en')
  })

  it('renders the selected language and switches it live (TC-C-02)', async () => {
    const user = userEvent.setup()
    render(
      <I18nProvider locale="fr" modules={['common', 'public']}>
        <Probe />
      </I18nProvider>,
    )

    await waitFor(() => {
      expect(screen.getByTestId('tagline').textContent).toBe(
        'Une seule passerelle pour tous les modèles',
      )
    })
    expect(document.documentElement.lang).toBe('fr')

    // TC-C-02：切换必须同步刷新 document.title；先用哨兵值证明它被重写。
    document.title = 'stale-title'
    await user.click(screen.getByRole('button', { name: 'switch' }))

    await waitFor(() => {
      expect(screen.getByTestId('locale').textContent).toBe('ja')
      expect(screen.getByTestId('tagline').textContent).toBe(
        'すべてのモデルをひとつのゲートウェイで',
      )
    })
    expect(document.documentElement.lang).toBe('ja')
    expect(document.title).toBe('NewTG')
    // 切换写入本地记忆（web-00 §7.3），刷新后沿用。
    expect(globalThis.localStorage.getItem('newtg.language')).toBe('ja')
  })
})
