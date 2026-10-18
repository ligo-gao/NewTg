import { create } from 'zustand'

import { useI18n } from '@/app/providers/i18n'

/**
 * Toast queue (modules-00 §4.8, web-00 §6.4).
 *
 * Durations: success 3s, info 5s, error 5s. The *consequence* explanation of a
 * danger action never auto-dismisses — it stays until the user closes it.
 *
 * Error toasts attach the `request_id` supplied by `api/errors.ts`. This module
 * deliberately keeps a structural parameter type so `shared/` does not import
 * `api/` (web-00 §2.2 import boundaries).
 */

export type ToastTone = 'success' | 'info' | 'error' | 'danger-consequence'

export const TOAST_DURATIONS = {
  success: 3000,
  info: 5000,
  error: 5000,
  'danger-consequence': null,
} as const satisfies Record<ToastTone, number | null>

export interface ToastInput {
  tone: ToastTone
  message: string
  /** Populated from `ApiError.request_id` by `pushErrorToast`. */
  requestId?: string
  /** Overrides `TOAST_DURATIONS`; `null` means "keep until dismissed". */
  duration?: number | null
}

export interface ToastItem {
  id: string
  tone: ToastTone
  message: string
  requestId?: string
  duration: number | null
}

interface ToastState {
  toasts: ToastItem[]
  push: (input: ToastInput) => string
  dismiss: (id: string) => void
}

const timers = new Map<string, ReturnType<typeof setTimeout>>()

function createToastId(): string {
  if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
    return crypto.randomUUID()
  }
  return `toast-${Date.now()}-${Math.random().toString(36).slice(2)}`
}

function clearTimer(id: string): void {
  const timer = timers.get(id)
  if (timer !== undefined) {
    clearTimeout(timer)
    timers.delete(id)
  }
}

export const useToastStore = create<ToastState>((set, get) => ({
  toasts: [],

  push: (input) => {
    const id = createToastId()
    const duration =
      input.duration === undefined ? TOAST_DURATIONS[input.tone] : input.duration

    const item: ToastItem = {
      id,
      tone: input.tone,
      message: input.message,
      duration,
    }
    if (input.requestId !== undefined) item.requestId = input.requestId

    set((state) => ({ toasts: [...state.toasts, item] }))

    if (duration !== null) {
      timers.set(
        id,
        setTimeout(() => {
          timers.delete(id)
          get().dismiss(id)
        }, duration),
      )
    }

    return id
  },

  dismiss: (id) => {
    clearTimer(id)
    set((state) => ({ toasts: state.toasts.filter((toast) => toast.id !== id) }))
  },
}))

/** Minimal structural view of `ApiError` used to build an error toast. */
export interface ApiErrorLike {
  title?: string
  detail?: string
  request_id?: string
}

/**
 * Pushes an error toast whose text and `request_id` come from the normalized
 * error layer. Keeping this helper structural preserves the `shared/` boundary.
 */
export function pushErrorToast(error: ApiErrorLike): string {
  return useToastStore.getState().push({
    tone: 'error',
    message: error.detail ?? error.title ?? 'Request failed',
    requestId: error.request_id,
  })
}

const TONE_CLASSES: Record<ToastTone, string> = {
  success: 'border-success-line',
  info: 'border-info-line',
  error: 'border-danger-line',
  'danger-consequence': 'border-danger-line',
}

const DOT_CLASSES: Record<ToastTone, string> = {
  success: 'bg-success',
  info: 'bg-info',
  error: 'bg-danger',
  'danger-consequence': 'bg-danger',
}

/**
 * Global toast viewport. Mounted once by the application providers; announces
 * politely and lets the user dismiss any toast, which is mandatory for the
 * non-auto-dismissing `danger-consequence` tone.
 */
export function ToastViewport() {
  const { t } = useI18n()
  const toasts = useToastStore((state) => state.toasts)
  const dismiss = useToastStore((state) => state.dismiss)

  if (toasts.length === 0) return null

  return (
    <div
      aria-live="polite"
      data-testid="toast-viewport"
      className="pointer-events-none fixed bottom-4 right-4 z-50 flex w-80 flex-col gap-2"
    >
      {toasts.map((toast) => (
        <div
          key={toast.id}
          role={toast.tone === 'success' || toast.tone === 'info' ? 'status' : 'alert'}
          data-testid="toast"
          data-tone={toast.tone}
          className={[
            'pointer-events-auto flex items-start gap-2 rounded-xl border bg-surface-card p-3 text-sm text-ink-1 shadow-[0_12px_32px_var(--overlay)]',
            TONE_CLASSES[toast.tone],
          ].join(' ')}
        >
          <span
            aria-hidden="true"
            className={[
              'mt-1.5 size-1.5 shrink-0 rounded-full',
              DOT_CLASSES[toast.tone],
            ].join(' ')}
          />
          <div className="min-w-0 flex-1">
            <p className="break-words">{toast.message}</p>
            {toast.requestId !== undefined ? (
              <p
                data-testid="toast-request-id"
                className="mt-1 font-mono text-[11px] leading-4 text-ink-3"
              >
                {t('common.toast.request_id', { requestId: toast.requestId })}
              </p>
            ) : null}
          </div>
          <button
            type="button"
            aria-label={t('common.toast.dismiss')}
            className="rounded p-0.5 text-ink-3 hover:text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
            onClick={() => dismiss(toast.id)}
          >
            <svg
              aria-hidden="true"
              viewBox="0 0 16 16"
              className="size-4"
              fill="none"
              stroke="currentColor"
              strokeWidth="1.5"
            >
              <path d="M4 4l8 8M12 4l-8 8" strokeLinecap="round" />
            </svg>
          </button>
        </div>
      ))}
    </div>
  )
}
