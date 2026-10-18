import { useState, type FormEvent } from 'react'

import { apiClient } from '@/api/client'
import { ApiError } from '@/api/errors'
import { requireData } from '@/api/result'
import { sanitizeAuthRedirect } from '@/lib/auth-redirect'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { FormAlert, TextField } from '@/shared/ui/form'

import { AuthPageShell } from './AuthPageShell'
import { OtpInput } from './OtpInput'

type ChallengeMode = 'totp' | 'backup'

export interface TwoFactorChallengePageProps {
  /** One-time ticket from the login response (`?ticket=`). */
  ticket?: string
  redirect?: string
  onNavigate?: (destination: string) => void
}

function defaultNavigate(destination: string): void {
  window.location.assign(destination)
}

function formatBackupCode(raw: string): string {
  const cleaned = raw.toUpperCase().replace(/[^A-Z0-9]/g, '').slice(0, 8)
  return cleaned.length > 4 ? `${cleaned.slice(0, 4)}-${cleaned.slice(4)}` : cleaned
}

const BACKUP_PATTERN = /^[A-Z0-9]{4}-[A-Z0-9]{4}$/

/**
 * F-01-08/F-01-09 challenge. Only the ticket identifies the user — the page
 * never sends a user id or username, matching the server contract.
 */
export function TwoFactorChallengePage({
  ticket,
  redirect,
  onNavigate = defaultNavigate,
}: TwoFactorChallengePageProps) {
  const { t } = useI18n()
  const [mode, setMode] = useState<ChallengeMode>('totp')
  const [code, setCode] = useState('')
  const [error, setError] = useState<string | undefined>(undefined)
  const [submitting, setSubmitting] = useState(false)

  if (ticket === undefined || ticket === '') {
    return (
      <AuthPageShell title={t('auth.two_factor.expired_title')}>
        <p className="text-sm text-ink-2">{t('auth.two_factor.expired_body')}</p>
        <Button className="mt-4 h-11 w-full" onClick={() => onNavigate('/sign-in')}>
          {t('auth.two_factor.to_sign_in')}
        </Button>
      </AuthPageShell>
    )
  }

  const submitDisabled =
    submitting ||
    (mode === 'totp' ? code.length !== 6 : !BACKUP_PATTERN.test(code))

  async function handleSubmit(event: FormEvent<HTMLFormElement>): Promise<void> {
    event.preventDefault()
    // Re-narrow here: the early return above does not narrow inside a closure.
    if (ticket === undefined || ticket === '') return
    setError(undefined)
    setSubmitting(true)
    try {
      const data = requireData(
        await apiClient.POST('/api/auth/2fa/challenge', {
          body: { ticket, code, type: mode },
        }),
      )
      if (data.user.must_change_password) {
        onNavigate('/security?force=password')
        return
      }
      onNavigate(sanitizeAuthRedirect(redirect))
    } catch (caught) {
      if (caught instanceof ApiError && caught.status === 429) {
        setError(t('auth.sign_in.error.rate_limited'))
      } else if (caught instanceof ApiError && caught.status === 401) {
        setError(t('auth.two_factor.error'))
        setCode('')
      } else {
        setError(t('auth.sign_in.error.unavailable'))
      }
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <AuthPageShell
      title={t('auth.two_factor.title')}
      subtitle={<span>{t('auth.two_factor.description')}</span>}
    >
      <form
        className="flex flex-col gap-4"
        onSubmit={(event) => void handleSubmit(event)}
        noValidate
      >
        {error !== undefined ? <FormAlert>{error}</FormAlert> : null}

        <div role="tablist" aria-label={t('auth.two_factor.title')} className="flex gap-4 border-b border-border-hairline">
          {(['totp', 'backup'] as const).map((value) => (
            <button
              key={value}
              type="button"
              role="tab"
              aria-selected={mode === value}
              className={[
                '-mb-px border-b-2 px-1 pb-2 text-[13px] font-medium focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none',
                mode === value
                  ? 'border-brand text-ink-1'
                  : 'border-transparent text-ink-3 hover:text-ink-1',
              ].join(' ')}
              onClick={() => {
                setMode(value)
                setCode('')
                setError(undefined)
              }}
            >
              {value === 'totp'
                ? t('auth.two_factor.code_tab')
                : t('auth.two_factor.backup_tab')}
            </button>
          ))}
        </div>

        <div role="tabpanel">
          {mode === 'totp' ? (
            <OtpInput
              value={code}
              onChange={setCode}
              ariaLabel={t('auth.two_factor.group_label')}
              disabled={submitting}
            />
          ) : (
            <TextField
              label={t('auth.two_factor.backup_label')}
              placeholder={t('auth.two_factor.backup_placeholder')}
              autoComplete="one-time-code"
              className="font-mono uppercase tracking-widest"
              value={code}
              onChange={(event) => {
                setCode(formatBackupCode(event.target.value))
              }}
            />
          )}
        </div>

        <Button type="submit" className="h-11 w-full" disabled={submitDisabled}>
          {submitting ? t('auth.two_factor.submitting') : t('auth.two_factor.submit')}
        </Button>
      </form>
    </AuthPageShell>
  )
}
