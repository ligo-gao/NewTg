import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import {
  TOAST_DURATIONS,
  pushErrorToast,
  useToastStore,
  type ToastTone,
} from './toast'

function toasts() {
  return useToastStore.getState().toasts
}

beforeEach(() => {
  vi.useFakeTimers()
  useToastStore.setState({ toasts: [] })
})

afterEach(() => {
  useToastStore.setState({ toasts: [] })
  vi.useRealTimers()
})

describe('TOAST_DURATIONS', () => {
  it('freezes the modules-00 §4.8 durations', () => {
    expect(TOAST_DURATIONS.success).toBe(3000)
    expect(TOAST_DURATIONS.info).toBe(5000)
    expect(TOAST_DURATIONS.error).toBe(5000)
  })
})

describe('useToastStore auto-dismissal', () => {
  it('unmounts a success toast after 3 seconds', () => {
    useToastStore.getState().push({ tone: 'success', message: 'Saved' })
    expect(toasts()).toHaveLength(1)

    vi.advanceTimersByTime(TOAST_DURATIONS.success - 1)
    expect(toasts()).toHaveLength(1)

    vi.advanceTimersByTime(1)
    expect(toasts()).toHaveLength(0)
  })

  it('unmounts an error toast after 5 seconds', () => {
    useToastStore.getState().push({ tone: 'error', message: 'Request failed' })
    expect(toasts()).toHaveLength(1)

    vi.advanceTimersByTime(TOAST_DURATIONS.error - 1)
    expect(toasts()).toHaveLength(1)

    vi.advanceTimersByTime(1)
    expect(toasts()).toHaveLength(0)
  })

  it('keeps a danger-consequence toast until it is dismissed manually', () => {
    const id = useToastStore
      .getState()
      .push({ tone: 'danger-consequence', message: 'This cannot be undone' })

    vi.advanceTimersByTime(60_000)
    expect(toasts()).toHaveLength(1)

    useToastStore.getState().dismiss(id)
    expect(toasts()).toHaveLength(0)
  })

  it('allows a caller to override the default duration', () => {
    useToastStore
      .getState()
      .push({ tone: 'info' as ToastTone, message: 'Heads up', duration: 1000 })

    vi.advanceTimersByTime(999)
    expect(toasts()).toHaveLength(1)

    vi.advanceTimersByTime(1)
    expect(toasts()).toHaveLength(0)
  })
})

describe('pushErrorToast', () => {
  it('carries the request_id supplied by the error layer', () => {
    pushErrorToast({
      title: 'Rate limited',
      detail: 'Too many requests',
      request_id: 'req-4242',
    })

    expect(toasts()).toHaveLength(1)
    expect(toasts()[0]?.tone).toBe('error')
    expect(toasts()[0]?.message).toBe('Too many requests')
    expect(toasts()[0]?.requestId).toBe('req-4242')
  })

  it('omits the request id when the error came without one', () => {
    pushErrorToast({ title: 'Internal error' })

    expect(toasts()[0]?.message).toBe('Internal error')
    expect(toasts()[0]?.requestId).toBeUndefined()
  })
})
