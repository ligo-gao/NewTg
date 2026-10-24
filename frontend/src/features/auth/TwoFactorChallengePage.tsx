import { useRef, useState, type FormEvent, type KeyboardEvent } from 'react'

import { apiClient } from '@/api/client'
import { ApiError } from '@/api/errors'
import { requireData } from '@/api/result'
import { sanitizeAuthRedirect } from '@/lib/auth-redirect'
import { useI18n } from '@/shared/lib/i18n-context'
import { useCountdown } from '@/shared/lib/use-countdown'
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
 * Problem type D2 emits for a malformed/expired/consumed ticket. A wrong TOTP
 * or backup code is a plain 400 (`bad_request`), so the page must branch on
 * this type instead of treating every 400 as a dead ticket — otherwise the
 * first typo replaces the form with the terminal expired screen.
 */
const TICKET_INVALID_TYPE = 'two_factor_ticket_invalid'

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
  // Only a 400 whose problem type is `two_factor_ticket_invalid` is terminal
  // (malformed/expired/consumed ticket). Any other 400 is a code mistake and
  // keeps the retry form on screen.
  const [expired, setExpired] = useState(false)
  const countdown = useCountdown()
  const tabRefs = useRef<Record<ChallengeMode, HTMLButtonElement | null>>({
    totp: null,
    backup: null,
  })

  function selectMode(next: ChallengeMode): void {
    setMode(next)
    setCode('')
    setError(undefined)
  }

  function handleTabKeyDown(
    event: KeyboardEvent<HTMLButtonElement>,
    current: ChallengeMode,
  ): void {
    if (event.key !== 'ArrowLeft' && event.key !== 'ArrowRight') return
    event.preventDefault()
    const next: ChallengeMode = current === 'totp' ? 'backup' : 'totp'
    selectMode(next)
    tabRefs.current[next]?.focus()
  }

  if (expired || ticket === undefined || ticket === '') {
    return (
      <AuthPageShell title={t('auth.two_factor.expired_title')}>
        <p className="text-body text-ink-2">{t('auth.two_factor.expired_body')}</p>
        <Button className="mt-4 h-11 w-full" onClick={() => onNavigate('/sign-in')}>
          {t('auth.two_factor.to_sign_in')}
        </Button>
      </AuthPageShell>
    )
  }

  const submitDisabled =
    submitting ||
    countdown.running ||
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
        // Server-driven retry window: the ticket stays valid, so wait exactly
        // as long as the gateway says instead of letting the user hammer it.
        countdown.start(caught.retry_after ?? 60)
        setError(t('auth.sign_in.error.rate_limited'))
      } else if (caught instanceof ApiError && caught.status === 401) {
        setError(t('auth.two_factor.error'))
        setCode('')
      } else if (caught instanceof ApiError && caught.status === 400) {
        if (caught.type.endsWith(TICKET_INVALID_TYPE)) {
          setExpired(true)
        } else {
          // Wrong TOTP/backup code: stay on the form so the visitor can retry.
          setError(t('auth.two_factor.error'))
          setCode('')
        }
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
              id={`two-factor-tab-${value}`}
              ref={(element) => {
                tabRefs.current[value] = element
              }}
              type="button"
              role="tab"
              aria-selected={mode === value}
              aria-controls="two-factor-panel"
              tabIndex={mode === value ? 0 : -1}
              className={[
                '-mb-px border-b-2 px-1 pb-2 text-meta font-medium focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none',
                mode === value
                  ? 'border-brand text-ink-1'
                  : 'border-transparent text-ink-3 hover:text-ink-1',
              ].join(' ')}
              onClick={() => {
                selectMode(value)
              }}
              onKeyDown={(event) => {
                handleTabKeyDown(event, value)
              }}
            >
              {value === 'totp'
                ? t('auth.two_factor.code_tab')
                : t('auth.two_factor.backup_tab')}
            </button>
          ))}
        </div>

        <div role="tabpanel" id="two-factor-panel" aria-labelledby={`two-factor-tab-${mode}`}>
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
          {submitting
            ? t('auth.two_factor.submitting')
            : countdown.running
              ? t('auth.two_factor.retry_in', { seconds: countdown.seconds })
              : t('auth.two_factor.submit')}
        </Button>
      </form>
    </AuthPageShell>
  )
}
