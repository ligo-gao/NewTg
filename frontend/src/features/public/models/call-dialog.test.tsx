import { fireEvent, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'

import type { PublicModelCard } from '@/features/public/pricing/api'
import { useToastStore } from '@/shared/ui/toast'
import { renderWithProviders } from '@/test-support/providers'

import { CallDialog } from './call-dialog'

afterEach(() => {
  // `Object.defineProperty` is not undone by vitest's `restoreMocks`; remove
  // the clipboard stub explicitly so it cannot leak into another suite.
  Reflect.deleteProperty(
    navigator as unknown as Record<string, unknown>,
    'clipboard',
  )
  vi.restoreAllMocks()
})

const MODEL: PublicModelCard = {
  model_id: 'openai/gpt-5',
  display_name: 'GPT-5',
  vendor: 'openai',
  capabilities: ['text'],
  context_length: 128000,
  max_output: 32000,
  mode: 'per_token',
  prompt_price: '500',
  completion_price: '1500',
  cache_read_price: null,
  cache_write_price: null,
  is_free: false,
  group_price: null,
  pricing_schedule: null,
  endpoints: ['chat.completions'],
  tags: [],
  description: '',
}

function stubClipboard() {
  const writeText = vi.fn().mockResolvedValue(undefined)
  Object.defineProperty(navigator, 'clipboard', {
    configurable: true,
    value: { writeText },
  })
  return writeText
}

describe('CallDialog', () => {
  it('opens with the model name and kind-specific endpoint', async () => {
    await renderWithProviders(
      <CallDialog
        open
        onOpenChange={vi.fn()}
        model={MODEL}
        kind="embedding"
        serverAddress="https://gateway.example.com"
        isLoggedIn
      />,
      ['public'],
    )

    expect(screen.getByRole('dialog')).toBeTruthy()
    expect(screen.getByRole('heading', { name: 'Call GPT-5' })).toBeTruthy()
    // Tablist has its own label instead of reusing the step-2 heading text.
    expect(screen.getByRole('tablist', { name: 'Call GPT-5' })).toBeTruthy()
    expect(screen.getByText('openai/gpt-5 · POST /embeddings')).toBeTruthy()
    expect(screen.getByText(/https:\/\/gateway\.example\.com\/v1\/embeddings/)).toBeTruthy()
  })

  it('hides the stream switch for non-chat models', async () => {
    const first = await renderWithProviders(
      <CallDialog
        open
        onOpenChange={vi.fn()}
        model={MODEL}
        kind="embedding"
        serverAddress="https://gateway.example.com"
        isLoggedIn
      />,
      ['public'],
    )
    expect(screen.queryByLabelText('Stream the response')).toBeNull()
    first.unmount()

    await renderWithProviders(
      <CallDialog
        open
        onOpenChange={vi.fn()}
        model={MODEL}
        kind="chat"
        serverAddress="https://gateway.example.com"
        isLoggedIn
      />,
      ['public'],
    )
    expect(screen.getByLabelText('Stream the response')).toBeTruthy()
  })

  it('links guests to sign-in and signed-in users to /tokens', async () => {
    const guest = await renderWithProviders(
      <CallDialog
        open
        onOpenChange={vi.fn()}
        model={MODEL}
        kind="chat"
        serverAddress="https://gateway.example.com"
        isLoggedIn={false}
      />,
      ['public'],
    )
    expect(
      screen
        .getByRole('link', { name: 'Sign in to create a key' })
        .getAttribute('href'),
    ).toBe('/sign-in?redirect=%2Ftokens')
    guest.unmount()

    await renderWithProviders(
      <CallDialog
        open
        onOpenChange={vi.fn()}
        model={MODEL}
        kind="chat"
        serverAddress="https://gateway.example.com"
        isLoggedIn
      />,
      ['public'],
    )
    expect(
      screen
        .getByRole('link', { name: 'Create a key in the console' })
        .getAttribute('href'),
    ).toBe('/tokens')
  })

  it('copies the active sample and calls the toast store', async () => {
    const writeText = stubClipboard()
    const push = vi
      .spyOn(useToastStore.getState(), 'push')
      .mockReturnValue('toast-1')

    await renderWithProviders(
      <CallDialog
        open
        onOpenChange={vi.fn()}
        model={MODEL}
        kind="chat"
        serverAddress="https://gateway.example.com"
        isLoggedIn
      />,
      ['public'],
    )

    fireEvent.click(screen.getByRole('tab', { name: 'Python' }))
    fireEvent.click(screen.getByRole('button', { name: 'Copy' }))

    await waitFor(() => {
      expect(push).toHaveBeenCalledTimes(1)
    })
    const copied = writeText.mock.calls[0]?.[0] as string
    expect(copied).toContain('from openai import OpenAI')
    expect(copied).toContain('<YOUR_TOKEN>')
    expect(copied).toContain('client.chat.completions.create')
    expect(copied).toContain('base_url="https://gateway.example.com/v1"')
    expect(push.mock.calls[0]?.[0]).toMatchObject({
      tone: 'success',
      message: 'Copied',
    })
  })

  it('keeps every step reachable inside a bounded scroll container', async () => {
    await renderWithProviders(
      <CallDialog
        open
        onOpenChange={vi.fn()}
        model={MODEL}
        kind="chat"
        serverAddress="https://gateway.example.com"
        isLoggedIn
      />,
      ['public'],
    )

    const scroll = screen.getByTestId('call-dialog-scroll')
    expect(scroll.className).toContain('overflow-y-auto')
    expect(scroll.className).toContain('max-h-[72vh]')
    // The response step scrolls with the body; the close control stays pinned
    // outside the scroll container.
    expect(scroll.textContent).toContain('Read the response')
    expect(
      scroll.contains(screen.getByRole('button', { name: 'Close' })),
    ).toBe(false)
  })

  it('closes from the labelled close button', async () => {
    const onOpenChange = vi.fn()
    await renderWithProviders(
      <CallDialog
        open
        onOpenChange={onOpenChange}
        model={MODEL}
        kind="chat"
        serverAddress="https://gateway.example.com"
        isLoggedIn
      />,
      ['public'],
    )

    fireEvent.click(screen.getByRole('button', { name: 'Close' }))
    expect(onOpenChange).toHaveBeenCalledWith(false)
  })
})
