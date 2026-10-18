import { cleanup, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it } from 'vitest'

import { I18nProvider, useI18n } from './i18n'

afterEach(cleanup)

function Probe() {
  const { t } = useI18n()
  return <p>{t('common.app.title')}</p>
}

describe('I18nProvider', () => {
  it('loads the app-wide common domain and re-renders when it lands', async () => {
    render(
      <I18nProvider>
        <Probe />
      </I18nProvider>,
    )

    // The loader is async, so the first paint falls back to the key itself;
    // the provider's revision bump must then replace it with real copy.
    expect(screen.getByText('common.app.title')).toBeTruthy()
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
})
