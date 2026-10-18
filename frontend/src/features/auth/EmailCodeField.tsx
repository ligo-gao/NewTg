import { useState } from 'react'

import { apiClient } from '@/api/client'
import { ApiError } from '@/api/errors'
import { useI18n } from '@/shared/lib/i18n-context'
import { useCountdown } from '@/shared/lib/use-countdown'
import { Button } from '@/shared/ui/button'
import { TextField } from '@/shared/ui/form'

export type EmailCodePurpose = 'register' | 'reset' | 'verify' | 'bind'

/**
 * Documented fallback cooldowns (modules-01 F-01-02): registration 60s and
 * password reset 30s. A server `Retry-After` always wins — the durations are
 * intentionally not hardcoded into the countdown itself.
 */
export const EMAIL_CODE_DEFAULT_COOLDOWN: Record<EmailCodePurpose, number> = {
  register: 60,
  reset: 30,
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
}: EmailCodeFieldProps) {
  const { t } = useI18n()
  const countdown = useCountdown()
  const [sending, setSending] = useState(false)
  const [error, setError] = useState<string | undefined>(undefined)

  const emailValid = EMAIL_PATTERN.test(email)
  const fallback = cooldownSeconds ?? EMAIL_CODE_DEFAULT_COOLDOWN[purpose]
  const buttonDisabled = disabled || sending || countdown.running || !emailValid

  async function requestCode(): Promise<void> {
    setError(undefined)
    setSending(true)
    try {
      await apiClient.POST('/api/auth/email-code', {
        body: { email, purpose },
      })
      countdown.start(fallback)
    } catch (caught) {
      if (caught instanceof ApiError && caught.status === 429) {
        // Server-driven duration: never guess when the gateway knows better.
        countdown.start(caught.retry_after ?? fallback)
      } else {
        setError(t('auth.email_code.failed'))
      }
    } finally {
      setSending(false)
    }
  }

  return (
    <div className="flex flex-col gap-1.5">
      <div className="flex items-end gap-2">
        <div className="flex-1">
          <TextField
            label={t('auth.email_code.label')}
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
        <Button
          variant="secondary"
          className="h-10 w-28 shrink-0 px-2 text-[13px]"
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
      </div>
      {!emailValid ? (
        <p className="text-xs text-ink-3">{t('auth.email_code.invalid_email')}</p>
      ) : null}
      {error !== undefined ? <p className="text-xs text-danger">{error}</p> : null}
    </div>
  )
}
