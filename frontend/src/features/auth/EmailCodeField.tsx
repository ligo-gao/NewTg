import { useState } from 'react'

import { apiClient } from '@/api/client'
import { ApiError } from '@/api/errors'
import type { components } from '@/api/schema'
import { usePublicStatus } from '@/lib/public-status'
import { useI18n } from '@/shared/lib/i18n-context'
import { useCountdown } from '@/shared/lib/use-countdown'
import { Button } from '@/shared/ui/button'
import { TextField } from '@/shared/ui/form'

import { TurnstileWidget } from './TurnstileWidget'

export type EmailCodePurpose = 'register' | 'reset' | 'verify' | 'bind'

/**
 * Documented fallback cooldowns (modules-01 F-01-02). Every email-code purpose
 * shares the same 60s server cooldown (`handlers_auth.go:46`); a server
 * `Retry-After` always wins — the durations are intentionally not hardcoded
 * into the countdown itself.
 */
export const EMAIL_CODE_DEFAULT_COOLDOWN: Record<EmailCodePurpose, number> = {
  register: 60,
  reset: 60,
  verify: 60,
  bind: 60,
}

const EMAIL_PATTERN = /^[^\s@]+@[^\s@]+\.[^\s@]+$/

export interface EmailCodeFieldProps {
  purpose: EmailCodePurpose
  email: string
  value: string
  onChange: (value: string) => void
  disabled?: boolean
  /** Overrides the purpose default when the server sends no Retry-After. */
  cooldownSeconds?: number
  /** `line` puts the resend control in the label row, like the hall's links. */
  variant?: 'default' | 'line'
}

/**
 * Email verification code input plus the inline resend button. The countdown
 * is server-driven: on 429 the remaining seconds come from `Retry-After`;
 * otherwise the purpose default applies so the button cannot be spammed.
 */
export function EmailCodeField({
  purpose,
  email,
  value,
  onChange,
  disabled = false,
  cooldownSeconds,
  variant = 'default',
}: EmailCodeFieldProps) {
  const { t } = useI18n()
  const countdown = useCountdown()
  const [sending, setSending] = useState(false)
  const [error, setError] = useState<string | undefined>(undefined)
  const [turnstileToken, setTurnstileToken] = useState<string | undefined>(undefined)
  const [turnstileReset, setTurnstileReset] = useState(0)

  const statusQuery = usePublicStatus()
  const turnstile = statusQuery.data?.auth.turnstile
  const turnstileEnabled = turnstile?.enabled ?? false

  const emailValid = EMAIL_PATTERN.test(email)
  const fallback = cooldownSeconds ?? EMAIL_CODE_DEFAULT_COOLDOWN[purpose]
  const buttonDisabled = disabled || sending || countdown.running || !emailValid

  async function requestCode(): Promise<void> {
    setError(undefined)
    if (turnstileEnabled && (turnstileToken === undefined || turnstileToken === '')) {
      setError(t('auth.turnstile.required'))
      return
    }
    setSending(true)
    try {
      const body: components['schemas']['EmailCodeRequest'] = { email, purpose }
      if (turnstileEnabled) body.turnstile_token = turnstileToken
      await apiClient.POST('/api/auth/email-code', { body })
      countdown.start(fallback)
    } catch (caught) {
      if (caught instanceof ApiError && caught.status === 429) {
        // Server-driven duration: never guess when the gateway knows better.
        countdown.start(caught.retry_after ?? fallback)
      } else {
        setError(t('auth.email_code.failed'))
      }
    } finally {
      if (turnstileEnabled) {
        // The gate verified (or at least consumed) the token; ask for a new
        // one so the post-cooldown resend is not rejected as token-less.
        setTurnstileToken(undefined)
        setTurnstileReset((value) => value + 1)
      }
      setSending(false)
    }
  }

  return (
    <div className="flex flex-col gap-1.5">
      <div className={variant === 'line' ? '' : 'flex items-end gap-2'}>
        <div className={variant === 'line' ? '' : 'flex-1'}>
          <TextField
            variant={variant}
            label={t('auth.email_code.label')}
            labelAction={
              variant === 'line' ? (
                <button
                  type="button"
                  className="auth-hall-code-action"
                  disabled={buttonDisabled}
                  aria-live="polite"
                  onClick={() => {
                    void requestCode()
                  }}
                >
                  {sending
                    ? t('auth.email_code.sending')
                    : countdown.running
                      ? t('auth.email_code.resend', { seconds: countdown.seconds })
                      : t('auth.email_code.send')}
                </button>
              ) : undefined
            }
            value={value}
            onChange={(event) => {
              onChange(event.target.value.replace(/\D/g, '').slice(0, 6))
            }}
            inputMode="numeric"
            autoComplete="one-time-code"
            maxLength={6}
            disabled={disabled}
          />
        </div>
        {variant === 'line' ? null : (
          <Button
            variant="secondary"
            className="h-10 w-28 shrink-0 px-2 text-meta"
            disabled={buttonDisabled}
            aria-live="polite"
            onClick={() => {
              void requestCode()
            }}
          >
            {sending
              ? t('auth.email_code.sending')
              : countdown.running
                ? t('auth.email_code.resend', { seconds: countdown.seconds })
                : t('auth.email_code.send')}
          </Button>
        )}
      </div>
      {turnstileEnabled ? (
        <TurnstileWidget
          siteKey={turnstile?.site_key ?? ''}
          onToken={setTurnstileToken}
          resetSignal={turnstileReset}
        />
      ) : null}
      {!emailValid ? (
        <p className="text-meta text-ink-3">{t('auth.email_code.invalid_email')}</p>
      ) : null}
      {error !== undefined ? (
        <p role="alert" className="text-meta text-danger">
          {error}
        </p>
      ) : null}
    </div>
  )
}
