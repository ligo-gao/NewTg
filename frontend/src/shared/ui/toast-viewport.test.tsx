import { act, cleanup, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it } from 'vitest'

import { I18nProvider } from '@/app/providers/i18n'

import { ToastViewport, useToastStore } from './toast'

beforeEach(() => {
  useToastStore.setState({ toasts: [] })
})

afterEach(() => {
  useToastStore.setState({ toasts: [] })
  cleanup()
})

/**
 * The viewport is mounted inside I18nProvider in the real app
 * (app/providers/app-providers.tsx); these tests pin that the two hardcoded M0
 * strings now come from `common.toast.*`.
 */
describe('ToastViewport i18n', () => {
  it('renders the request_id label and dismiss label from the dictionary', async () => {
    render(
      <I18nProvider>
        <ToastViewport />
      </I18nProvider>,
    )

    act(() => {
      useToastStore.getState().push({
        tone: 'danger-consequence', // null duration: no timer outlives the test
        message: 'This cannot be undone',
        requestId: 'req-7',
      })
    })

    await waitFor(() => {
      expect(screen.getByTestId('toast-request-id').textContent).toBe(
        'request_id: req-7',
      )
    })
    const dismiss = screen.getByRole('button', {
      name: 'Dismiss notification',
    })
    expect(dismiss.getAttribute('aria-label')).toBe('Dismiss notification')
  })
})
