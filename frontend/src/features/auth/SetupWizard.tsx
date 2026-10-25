import { useState, type FormEvent } from 'react'

import { apiClient } from '@/api/client'
import { ApiError } from '@/api/errors'
import type { components } from '@/api/schema'
import { passwordPolicyProblem } from '@/lib/password-policy'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { FormAlert, PasswordField, TextField } from '@/shared/ui/form'

import { AuthHallShell } from './AuthHallShell'

const EMAIL_PATTERN = /^[^\s@]+@[^\s@]+\.[^\s@]+$/

type Step = 'site' | 'admin' | 'review'

const STEP_ORDER: Step[] = ['site', 'admin', 'review']

export interface SetupWizardProps {
  onNavigate?: (destination: string) => void
}

function defaultNavigate(destination: string): void {
  window.location.assign(destination)
}

function isHttpUrl(value: string): boolean {
  try {
    const url = new URL(value)
    return url.protocol === 'http:' || url.protocol === 'https:'
  } catch {
    return false
  }
}

/** `alice@example.com` → `a***@example.com` (review step shows no full address). */
function maskEmail(value: string): string {
  const [local, domain] = value.split('@')
  if (local === undefined || domain === undefined || local === '') return value
  return `${local.slice(0, 1)}***@${domain}`
}

/**
 * F-01-20 first-run wizard. The frozen request body is flat:
 * `site_title/server_address/username/email/password/confirmPassword`
 * (no nesting, no email_code/agree_terms — setup is pre-registration).
 */
export function SetupWizard({ onNavigate = defaultNavigate }: SetupWizardProps) {
  const { t } = useI18n()
  const [step, setStep] = useState<Step>('site')
  const [siteTitle, setSiteTitle] = useState('NEWTG')
  const [serverAddress, setServerAddress] = useState('')
  const [username, setUsername] = useState('')
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [confirmPassword, setConfirmPassword] = useState('')
  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({})
  const [formError, setFormError] = useState<string | undefined>(undefined)
  const [alreadyInitialized, setAlreadyInitialized] = useState(false)
  const [submitting, setSubmitting] = useState(false)

  const siteValid = siteTitle.trim() !== '' && isHttpUrl(serverAddress)
  const adminValid =
    username.trim() !== '' &&
    EMAIL_PATTERN.test(email) &&
    passwordPolicyProblem(password) === undefined &&
    password === confirmPassword

  function stepIndex(value: Step): number {
    return STEP_ORDER.indexOf(value) + 1
  }

  function next(): void {
    const errors: Record<string, string> = {}
    if (step === 'site') {
      if (siteTitle.trim() === '') errors.siteTitle = t('auth.validation.required')
      else if (siteTitle.trim().length > 64) {
        errors.siteTitle = t('auth.validation.site_title_length')
      }
      if (!isHttpUrl(serverAddress)) {
        errors.serverAddress = t('auth.validation.server_address')
      }
    } else if (step === 'admin') {
      if (username.trim() === '') errors.username = t('auth.validation.required')
      if (!EMAIL_PATTERN.test(email)) errors.email = t('auth.validation.email')
      if (passwordPolicyProblem(password) !== undefined) {
        errors.password = t('auth.validation.password_length')
      }
      if (password !== confirmPassword) {
        errors.confirmPassword = t('auth.validation.password_mismatch')
      }
    }
    setFieldErrors(errors)
    if (Object.keys(errors).length > 0) return

    if (step === 'site') setStep('admin')
    else if (step === 'admin') setStep('review')
  }

  async function finish(event: FormEvent<HTMLFormElement>): Promise<void> {
    event.preventDefault()
    setFormError(undefined)
    setSubmitting(true)
    try {
      const body: components['schemas']['SetupRequest'] = {
        site_title: siteTitle.trim(),
        server_address: serverAddress.trim(),
        username: username.trim(),
        email: email.trim(),
        password,
        confirmPassword,
      }
      await apiClient.POST('/api/setup', { body })
      onNavigate('/dashboard')
    } catch (caught) {
      if (
        caught instanceof ApiError &&
        caught.type.endsWith('setup_already_completed')
      ) {
        // Only the idempotency-anchor problem means "already initialized"; a
        // username/email conflict is also a 409 and must stay on the form.
        setAlreadyInitialized(true)
      } else {
        setFormError(t('auth.setup.error'))
      }
    } finally {
      setSubmitting(false)
    }
  }

  if (alreadyInitialized) {
    return (
      <AuthHallShell title={t('auth.setup.already_title')} variant="setup">
        <div className="auth-hall-stack">
          <p className="auth-hall-pane-lead">{t('auth.setup.already_body')}</p>
          <Button
            className="auth-hall-submit w-full"
            onClick={() => onNavigate('/sign-in')}
          >
            {t('auth.setup.sign_in')}
          </Button>
        </div>
      </AuthHallShell>
    )
  }

  return (
    <AuthHallShell title={t('auth.setup.title')} variant="setup">
      <ol className="auth-hall-steps" aria-label={t('auth.setup.title')}>
        {STEP_ORDER.map((value, index) => {
          const current = step === value
          const done = stepIndex(value) < stepIndex(step)
          return (
            <li
              key={value}
              aria-current={current ? 'step' : undefined}
              className="auth-hall-step"
              data-state={current ? 'current' : done ? 'done' : 'todo'}
            >
              <span aria-hidden="true" className="auth-hall-step-dot">
                {index + 1}
              </span>
              <span className="auth-hall-step-label">
                {value === 'site'
                  ? t('auth.setup.step_site')
                  : value === 'admin'
                    ? t('auth.setup.step_admin')
                    : t('auth.setup.step_review')}
              </span>
              {index < STEP_ORDER.length - 1 ? (
                <span aria-hidden="true" className="auth-hall-step-sep">
                  /
                </span>
              ) : null}
            </li>
          )
        })}
      </ol>

      {formError !== undefined ? <FormAlert>{formError}</FormAlert> : null}

      {step === 'site' ? (
        <div className="auth-hall-stack">
          <TextField
            variant="line"
            label={t('auth.setup.site_title')}
            maxLength={64}
            value={siteTitle}
            onChange={(event) => {
              setSiteTitle(event.target.value)
            }}
            error={fieldErrors.siteTitle}
            required
          />
          <TextField
            variant="line"
            label={t('auth.setup.server_address')}
            type="url"
            placeholder="https://gateway.example.com"
            hint={t('auth.setup.server_address_hint')}
            value={serverAddress}
            onChange={(event) => {
              setServerAddress(event.target.value)
            }}
            error={fieldErrors.serverAddress}
            required
          />
          <Button
            className="auth-hall-submit w-full"
            disabled={!siteValid}
            onClick={next}
          >
            {t('auth.setup.next')}
          </Button>
        </div>
      ) : step === 'admin' ? (
        <div className="auth-hall-stack">
          <TextField
            variant="line"
            label={t('auth.setup.username')}
            autoComplete="username"
            value={username}
            onChange={(event) => {
              setUsername(event.target.value)
            }}
            error={fieldErrors.username}
            required
          />
          <TextField
            variant="line"
            label={t('auth.setup.email')}
            type="email"
            autoComplete="email"
            value={email}
            onChange={(event) => {
              setEmail(event.target.value)
            }}
            error={fieldErrors.email}
            required
          />
          <PasswordField
            variant="line"
            label={t('auth.setup.password')}
            showLabel={t('auth.sign_in.show_password')}
            hideLabel={t('auth.sign_in.hide_password')}
            autoComplete="new-password"
            value={password}
            onChange={(event) => {
              setPassword(event.target.value)
            }}
            error={fieldErrors.password}
            required
          />
          <PasswordField
            variant="line"
            label={t('auth.setup.confirm_password')}
            showLabel={t('auth.sign_in.show_password')}
            hideLabel={t('auth.sign_in.hide_password')}
            autoComplete="new-password"
            value={confirmPassword}
            onChange={(event) => {
              setConfirmPassword(event.target.value)
            }}
            error={fieldErrors.confirmPassword}
            required
          />
          <div className="auth-hall-actions">
            <Button
              variant="secondary"
              className="auth-hall-secondary"
              onClick={() => setStep('site')}
            >
              {t('auth.setup.back')}
            </Button>
            <Button
              className="auth-hall-submit"
              disabled={!adminValid}
              onClick={next}
            >
              {t('auth.setup.next')}
            </Button>
          </div>
        </div>
      ) : (
        <form onSubmit={(event) => void finish(event)}>
          <div className="auth-hall-review">
            <p>{t('auth.setup.review_site', { value: siteTitle.trim() })}</p>
            <p>{t('auth.setup.review_server', { value: serverAddress.trim() })}</p>
            <p>{t('auth.setup.review_admin', { value: username.trim() })}</p>
            <p>{t('auth.setup.review_email', { value: maskEmail(email.trim()) })}</p>
          </div>
          <div className="auth-hall-actions">
            <Button
              variant="secondary"
              className="auth-hall-secondary"
              onClick={() => setStep('admin')}
            >
              {t('auth.setup.back')}
            </Button>
            <Button
              type="submit"
              className="auth-hall-submit"
              disabled={submitting}
            >
              {submitting ? t('auth.setup.finishing') : t('auth.setup.finish')}
            </Button>
          </div>
        </form>
      )}
    </AuthHallShell>
  )
}
