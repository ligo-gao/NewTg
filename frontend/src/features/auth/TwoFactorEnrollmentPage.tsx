import { useEffect, useState } from 'react'
import { QRCodeSVG } from 'qrcode.react'

import { toApiError } from '@/api/client'
import { ApiError } from '@/api/errors'
import { sanitizeAuthRedirect } from '@/lib/auth-redirect'
import { useI18n } from '@/shared/lib/i18n-context'
import { useCountdown } from '@/shared/lib/use-countdown'
import { Button } from '@/shared/ui/button'
import { FormAlert } from '@/shared/ui/form'

import { AuthHallShell } from './AuthHallShell'
import { OtpInput } from './OtpInput'

export interface TwoFactorEnrollmentPageProps {
  /** One-time enrollment ticket from the login response (`?ticket=`). */
  ticket?: string
  redirect?: string
  onNavigate?: (destination: string) => void
}

function defaultNavigate(destination: string): void {
  // Full document navigation: the enrollment API sets the session cookie, and
  // a reload is what guarantees every later request (and /api/me) uses it.
  window.location.assign(destination)
}

/**
 * Problem type for a malformed/expired/consumed enrollment ticket. A wrong
 * TOTP code is a plain 400 (`bad_request`), so only this type is terminal.
 */
const TICKET_INVALID_TYPE = 'two_factor_ticket_invalid'

interface EnrollSetupResponse {
  secret: string
  otpauth_url: string
}

interface EnrollEnableResponse {
  status: string
  backup_codes: string[]
}

interface EnrollConfirmResponse {
  user: { must_change_password?: boolean }
}

/**
 * The enrollment endpoints are outside the generated OpenAPI contract (the
 * spec file is owned by another change), so this page talks to them through a
 * small fetch wrapper and normalizes failures with the same `toApiError`
 * helper the contract client uses.
 */
async function postEnroll<T>(path: string, body: unknown): Promise<T> {
  const response = await fetch(`/api/auth/2fa/enroll/${path}`, {
    method: 'POST',
    credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
    body: JSON.stringify(body),
  })
  if (!response.ok) throw await toApiError(response)
  return (await response.json()) as T
}

function isTicketInvalid(error: unknown): boolean {
  return error instanceof ApiError && error.status === 400 && error.type.endsWith(TICKET_INVALID_TYPE)
}

function downloadBackupCodes(codes: readonly string[], title: string): void {
  const lines = [title, '', ...codes, '']
  const blob = new Blob([lines.join('\n')], { type: 'text/plain' })
  const url = URL.createObjectURL(blob)
  const link = document.createElement('a')
  link.href = url
  link.download = 'two-factor-backup-codes.txt'
  link.click()
  URL.revokeObjectURL(url)
}

type Step = 'scan' | 'verify' | 'codes'

/**
 * R-04 管理员强制绑定：登录返回 `mfa_enrollment_required` 后进入本页。
 *
 * 流程与 TwoFaSetupDialog 一致（scan → verify → codes），差别有两点：
 *   1. 凭据是登录下发的 scope=enroll 一次性 ticket，没有会话；
 *   2. 看到备用码之后必须显式确认，前端才调用 confirm 让服务端置
 *      totp_enabled=true 并签发正式会话（R-04 第 4 条）。
 */
export function TwoFactorEnrollmentPage({
  ticket,
  redirect,
  onNavigate = defaultNavigate,
}: TwoFactorEnrollmentPageProps) {
  const { t } = useI18n()
  const [step, setStep] = useState<Step>('scan')
  const [secret, setSecret] = useState('')
  const [otpauthUrl, setOtpauthUrl] = useState('')
  const [code, setCode] = useState('')
  const [backupCodes, setBackupCodes] = useState<string[]>([])
  const [stored, setStored] = useState(false)
  const [loading, setLoading] = useState(false)
  const [expired, setExpired] = useState(false)
  const [error, setError] = useState<string | undefined>(undefined)
  const countdown = useCountdown()

  const hasTicket = ticket !== undefined && ticket !== ''

  useEffect(() => {
    if (!hasTicket) return
    let cancelled = false
    setStep('scan')
    setLoading(true)
    setExpired(false)
    setError(undefined)

    void (async () => {
      try {
        const data = await postEnroll<EnrollSetupResponse>('setup', { ticket })
        if (cancelled) return
        setSecret(data.secret)
        setOtpauthUrl(data.otpauth_url)
      } catch (caught) {
        if (cancelled) return
        if (isTicketInvalid(caught)) {
          setExpired(true)
        } else if (caught instanceof ApiError && caught.status === 429) {
          countdown.start(caught.retry_after ?? 60)
          setError(t('auth.mfa_enroll.rate_limited'))
        } else {
          setError(t('auth.mfa_enroll.error'))
        }
      } finally {
        if (!cancelled) setLoading(false)
      }
    })()

    return () => {
      cancelled = true
    }
    // `t` is stable per locale; the setup request must only fire per ticket.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [ticket])

  function handleApiError(caught: unknown): void {
    if (isTicketInvalid(caught)) {
      setExpired(true)
    } else if (caught instanceof ApiError && caught.status === 429) {
      countdown.start(caught.retry_after ?? 60)
      setError(t('auth.mfa_enroll.rate_limited'))
    } else if (caught instanceof ApiError && caught.status === 400) {
      setError(t('auth.mfa_enroll.code_error'))
      setCode('')
    } else if (caught instanceof ApiError && caught.status === 409) {
      // Another tab/request already finished the enrollment.
      onNavigate(sanitizeAuthRedirect(redirect))
    } else {
      setError(t('auth.mfa_enroll.unavailable'))
    }
  }

  async function enable(): Promise<void> {
    if (!hasTicket) return
    setError(undefined)
    setLoading(true)
    try {
      const data = await postEnroll<EnrollEnableResponse>('enable', {
        ticket,
        secret,
        code,
      })
      setBackupCodes(data.backup_codes)
      setStep('codes')
    } catch (caught) {
      handleApiError(caught)
    } finally {
      setLoading(false)
    }
  }

  async function confirm(): Promise<void> {
    if (!hasTicket) return
    setError(undefined)
    setLoading(true)
    try {
      const data = await postEnroll<EnrollConfirmResponse>('confirm', {
        ticket,
        backup_codes_confirmed: true,
      })
      if (data.user.must_change_password) {
        // 与服务端受限会话一致：临时口令必须先改密才能进入控制台。
        onNavigate('/security?force=password')
        return
      }
      onNavigate(sanitizeAuthRedirect(redirect))
    } catch (caught) {
      handleApiError(caught)
    } finally {
      setLoading(false)
    }
  }

  if (expired || !hasTicket) {
    return (
      <AuthHallShell title={t('auth.mfa_enroll.expired_title')}>
        <div className="auth-hall-stack">
          <p className="auth-hall-pane-lead">{t('auth.mfa_enroll.expired_body')}</p>
          <Button className="auth-hall-submit w-full" onClick={() => onNavigate('/sign-in')}>
            {t('auth.mfa_enroll.to_sign_in')}
          </Button>
        </div>
      </AuthHallShell>
    )
  }

  const verifyDisabled = loading || countdown.running || code.length !== 6

  return (
    <AuthHallShell
      title={t('auth.mfa_enroll.title')}
      subtitle={t('auth.mfa_enroll.description')}
    >
      <div className="auth-hall-stack">
        {error !== undefined ? <FormAlert>{error}</FormAlert> : null}

        {loading && secret === '' && step === 'scan' ? (
          <p className="text-ink-3">{t('auth.mfa_enroll.loading')}</p>
        ) : step === 'scan' ? (
          <>
            <h2 className="text-body font-semibold text-ink-1">
              {t('auth.mfa_enroll.scan_title')}
            </h2>
            <p className="text-meta text-ink-2">
              {t('auth.mfa_enroll.scan_description')}
            </p>
            <div className="flex items-center gap-4">
              <div className="shrink-0 rounded-lg border border-border-hairline bg-surface-card p-2">
                <QRCodeSVG
                  value={otpauthUrl}
                  size={160}
                  level="M"
                  marginSize={2}
                  title={t('auth.mfa_enroll.scan_title')}
                />
              </div>
              <p className="text-meta text-ink-2">
                {t('auth.mfa_enroll.scan_manual_hint')}
              </p>
            </div>
            <label className="text-meta font-medium text-ink-1" htmlFor="mfa-enroll-secret">
              {t('auth.mfa_enroll.secret')}
            </label>
            <input
              id="mfa-enroll-secret"
              readOnly
              value={secret}
              className="h-10 w-full rounded-lg border border-border-default bg-surface-subtle px-3 font-mono text-meta text-ink-1"
            />
            <Button
              className="auth-hall-submit w-full"
              disabled={loading || secret === ''}
              onClick={() => setStep('verify')}
            >
              {t('auth.mfa_enroll.next')}
            </Button>
          </>
        ) : step === 'verify' ? (
          <>
            <h2 className="text-body font-semibold text-ink-1">
              {t('auth.mfa_enroll.verify_title')}
            </h2>
            <p className="text-meta text-ink-2">
              {t('auth.mfa_enroll.verify_description')}
            </p>
            <OtpInput
              value={code}
              onChange={setCode}
              ariaLabel={t('auth.two_factor.group_label')}
              disabled={loading}
            />
            <Button
              className="auth-hall-submit w-full"
              disabled={verifyDisabled}
              onClick={() => void enable()}
            >
              {loading
                ? t('auth.mfa_enroll.loading')
                : countdown.running
                  ? t('auth.mfa_enroll.retry_in', { seconds: countdown.seconds })
                  : t('auth.mfa_enroll.verify')}
            </Button>
            <Button variant="secondary" onClick={() => setStep('scan')}>
              {t('auth.mfa_enroll.back')}
            </Button>
          </>
        ) : (
          <>
            <h2 className="text-body font-semibold text-ink-1">
              {t('auth.mfa_enroll.codes_title')}
            </h2>
            <p className="text-meta text-ink-2">
              {t('auth.mfa_enroll.codes_description')}
            </p>
            <ul className="grid grid-cols-2 gap-2 rounded-lg bg-surface-subtle p-3 font-mono text-meta">
              {backupCodes.map((value) => (
                <li key={value}>{value}</li>
              ))}
            </ul>
            <Button
              variant="secondary"
              onClick={() => downloadBackupCodes(backupCodes, t('auth.mfa_enroll.codes_title'))}
            >
              {t('auth.mfa_enroll.download')}
            </Button>
            <label className="flex items-start gap-2 text-meta text-ink-2">
              <input
                type="checkbox"
                className="mt-0.5 size-4 accent-brand"
                checked={stored}
                onChange={(event) => setStored(event.target.checked)}
              />
              <span>{t('auth.mfa_enroll.stored')}</span>
            </label>
            <Button
              className="auth-hall-submit w-full"
              disabled={!stored || loading}
              onClick={() => void confirm()}
            >
              {t('auth.mfa_enroll.finish')}
            </Button>
          </>
        )}
      </div>
    </AuthHallShell>
  )
}
