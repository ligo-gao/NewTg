import { useState, type FormEvent } from 'react'

import { apiClient } from '@/api/client'
import { ApiError } from '@/api/errors'
import { requireData } from '@/api/result'
import { sanitizeAuthRedirect } from '@/lib/auth-redirect'
import {
  buildLoginCredentials,
  LoginEncryptionUnavailableError,
} from '@/lib/password-encryption'
import { useI18n, type TranslateFn } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { FormAlert, PasswordField, TextField } from '@/shared/ui/form'

import { AuthPageShell } from './AuthPageShell'

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

  async function handleSubmit(event: FormEvent<HTMLFormElement>): Promise<void> {
    event.preventDefault()
    setError(undefined)
    setSubmitting(true)
    try {
      const credentials = await buildLoginCredentials(identifier.trim(), password)
      const data = requireData(
        await apiClient.POST('/api/auth/login', { body: credentials }),
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
      setError(signInErrorMessage(caught, t))
    } finally {
      setSubmitting(false)
    }
  }

  const site = siteTitle ?? t('auth.shell.brand')

  return (
    <AuthPageShell title={t('auth.sign_in.title', { site })}>
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
            className="text-[13px] font-medium text-brand hover:underline"
          >
            {t('auth.sign_in.forgot')}
          </a>
        </div>

        <Button
          type="submit"
          className="h-11 w-full"
          disabled={submitting || identifier.trim() === '' || password === ''}
        >
          {submitting ? t('auth.sign_in.submitting') : t('auth.sign_in.submit')}
        </Button>

        <p className="text-center text-[13px] text-ink-2">
          {t('auth.sign_in.no_account')}{' '}
          <a className="font-medium text-brand hover:underline" href="/sign-up">
            {t('auth.sign_in.sign_up')}
          </a>
        </p>
      </form>
    </AuthPageShell>
  )
}
