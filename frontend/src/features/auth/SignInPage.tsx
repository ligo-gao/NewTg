import { useEffect, useRef, useState, type FormEvent } from 'react'
import { useQuery } from '@tanstack/react-query'

import { apiClient } from '@/api/client'
import { ApiError } from '@/api/errors'
import { requireData } from '@/api/result'
import { sanitizeAuthRedirect } from '@/lib/auth-redirect'
import {
  buildLoginCredentials,
  LoginEncryptionUnavailableError,
} from '@/lib/password-encryption'
import { usePublicStatus } from '@/lib/public-status'
import { meQueryOptions } from '@/lib/session'
import { useI18n, type TranslateFn } from '@/shared/lib/i18n-context'
import { useCountdown } from '@/shared/lib/use-countdown'
import { Button } from '@/shared/ui/button'
import { FormAlert, PasswordField, TextField } from '@/shared/ui/form'

import { AuthPageShell } from './AuthPageShell'
import { TurnstileWidget } from './TurnstileWidget'

export interface SignInPageProps {
  /** Sanitized `?redirect=` target from the route. */
  redirect?: string
  /** Injected by the route (and by tests) to keep this component router-free. */
  onNavigate?: (destination: string) => void
  siteTitle?: string
}

function defaultNavigate(destination: string): void {
  // A document navigation is intentional on auth boundaries: it guarantees the
  // new session cookie is used by every subsequent request and clears any
  // in-memory state from the previous identity.
  window.location.assign(destination)
}

function signInErrorMessage(error: unknown, t: TranslateFn): string {
  if (error instanceof LoginEncryptionUnavailableError) {
    return t('auth.sign_in.error.unavailable')
  }
  if (error instanceof ApiError) {
    if (error.status === 429) return t('auth.sign_in.error.rate_limited')
    if (error.status === 401) {
      if (error.type.endsWith('/account_disabled')) {
        return t('auth.sign_in.error.disabled')
      }
      if (error.type.endsWith('/temp_password_expired')) {
        return t('auth.sign_in.error.temp_expired')
      }
      return t('auth.sign_in.error.invalid')
    }
    if (error.status >= 500 || error.status === 0) {
      return t('auth.sign_in.error.unavailable')
    }
  }
  return t('auth.sign_in.error.generic')
}

/**
 * F-01-03 password sign-in. The encryption decision belongs to the server:
 * `buildLoginCredentials` returns a plaintext body only when the key endpoint
 * explicitly says `enabled: false`, and throws otherwise (anti-downgrade).
 */
export function SignInPage({
  redirect,
  onNavigate = defaultNavigate,
  siteTitle,
}: SignInPageProps) {
  const { t } = useI18n()
  const [identifier, setIdentifier] = useState('')
  const [password, setPassword] = useState('')
  const [error, setError] = useState<string | undefined>(undefined)
  const [submitting, setSubmitting] = useState(false)
  const countdown = useCountdown()
  const [turnstileToken, setTurnstileToken] = useState<string | undefined>(undefined)
  const [turnstileReset, setTurnstileReset] = useState(0)

  const statusQuery = usePublicStatus()
  const turnstile = statusQuery.data?.auth.turnstile
  const turnstileEnabled = turnstile?.enabled ?? false

  // An authenticated visitor must never see the sign-in form again.
  const meQuery = useQuery(meQueryOptions())
  const alreadyNavigated = useRef(false)
  useEffect(() => {
    if (meQuery.data !== undefined && !alreadyNavigated.current) {
      alreadyNavigated.current = true
      onNavigate('/dashboard')
    }
  }, [meQuery.data, onNavigate])

  async function handleSubmit(event: FormEvent<HTMLFormElement>): Promise<void> {
    event.preventDefault()
    if (turnstileEnabled && (turnstileToken === undefined || turnstileToken === '')) {
      // Turnstile enabled means the server rejects a token-less login with
      // 403; fail here instead of spending an attempt.
      setError(t('auth.turnstile.required'))
      return
    }
    setError(undefined)
    setSubmitting(true)
    try {
      const credentials = await buildLoginCredentials(identifier.trim(), password)
      const data = requireData(
        await apiClient.POST('/api/auth/login', {
          body: turnstileEnabled
            ? { ...credentials, turnstile_token: turnstileToken }
            : credentials,
        }),
      )

      if ('status' in data) {
        const params = new URLSearchParams({ ticket: data.ticket })
        if (redirect !== undefined && redirect !== '') {
          params.set('redirect', redirect)
        }
        onNavigate(`/two-factor-challenge?${params.toString()}`)
        return
      }

      if (data.user.must_change_password) {
        // Backend restricted session: only the change-password route is usable
        // until `POST /api/password` succeeds.
        onNavigate('/security?force=password')
        return
      }

      onNavigate(sanitizeAuthRedirect(redirect))
    } catch (caught) {
      if (caught instanceof ApiError && caught.status === 429) {
        // Server-driven lockout: the button stays disabled for exactly the
        // advertised retry window instead of a hardcoded guess.
        countdown.start(caught.retry_after ?? 60)
      }
      setError(signInErrorMessage(caught, t))
    } finally {
      if (turnstileEnabled) {
        // Turnstile tokens are single-use and a failed login already consumed
        // this one: clear it and restart the challenge.
        setTurnstileToken(undefined)
        setTurnstileReset((value) => value + 1)
      }
      setSubmitting(false)
    }
  }

  const site = siteTitle ?? t('auth.shell.brand')
  const passwordLoginEnabled =
    statusQuery.data?.auth.password_login_enabled ?? true

  return (
    <AuthPageShell title={t('auth.sign_in.title', { site })}>
      {statusQuery.isError ? (
        // Without status we cannot know whether Turnstile is required; the
        // form would submit a guaranteed-403 request in the enabled case, so
        // fail visibly instead of guessing "disabled".
        <div className="flex flex-col gap-3">
          <FormAlert>{t('auth.sign_in.status_error')}</FormAlert>
          <Button
            variant="secondary"
            className="h-10"
            onClick={() => void statusQuery.refetch()}
          >
            {t('common.action.retry')}
          </Button>
        </div>
      ) : statusQuery.isPending ? (
        <div className="space-y-3" aria-busy="true">
          <div className="h-10 rounded-lg bg-surface-subtle" />
          <div className="h-10 rounded-lg bg-surface-subtle" />
          <div className="h-11 rounded-lg bg-surface-subtle" />
        </div>
      ) : !passwordLoginEnabled ? (
        <FormAlert tone="warning">{t('auth.sign_in.password_disabled')}</FormAlert>
      ) : (
        <form
          className="flex flex-col gap-4"
          onSubmit={(event) => void handleSubmit(event)}
          noValidate
        >
          {error !== undefined ? <FormAlert>{error}</FormAlert> : null}

          <TextField
            label={t('auth.sign_in.identifier')}
            autoComplete="username"
            autoFocus
            value={identifier}
            onChange={(event) => {
              setIdentifier(event.target.value)
            }}
            required
          />

          <PasswordField
            label={t('auth.sign_in.password')}
            showLabel={t('auth.sign_in.show_password')}
            hideLabel={t('auth.sign_in.hide_password')}
            autoComplete="current-password"
            value={password}
            onChange={(event) => {
              setPassword(event.target.value)
            }}
            required
          />

          <div className="-mt-2 flex justify-end">
            <a
              href="/forgot-password"
              className="text-meta font-medium text-brand hover:underline"
            >
              {t('auth.sign_in.forgot')}
            </a>
          </div>

          {turnstileEnabled ? (
            <TurnstileWidget
              siteKey={turnstile?.site_key ?? ''}
              onToken={setTurnstileToken}
              resetSignal={turnstileReset}
            />
          ) : null}

          <Button
            type="submit"
            className="h-11 w-full"
            disabled={
              submitting ||
              countdown.running ||
              identifier.trim() === '' ||
              password === ''
            }
          >
            {submitting
              ? t('auth.sign_in.submitting')
              : countdown.running
                ? t('auth.sign_in.retry_in', { seconds: countdown.seconds })
                : t('auth.sign_in.submit')}
          </Button>

          <p className="text-center text-meta text-ink-2">
            {t('auth.sign_in.no_account')}{' '}
            <a className="font-medium text-brand hover:underline" href="/sign-up">
              {t('auth.sign_in.sign_up')}
            </a>
          </p>
        </form>
      )}
    </AuthPageShell>
  )
}
