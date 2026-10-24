import { useState, type FormEvent } from 'react'

import { apiClient } from '@/api/client'
import { ApiError } from '@/api/errors'
import { requireData } from '@/api/result'
import { useI18n } from '@/shared/lib/i18n-context'
import { formatCredits } from '@/shared/lib/format'
import { Button } from '@/shared/ui/button'
import { TextField } from '@/shared/ui/form'
import { useToastStore } from '@/shared/ui/toast'

export interface RedeemPageProps {
  /** Server config `enable_redemption`; the section is hidden when false. */
  enabled?: boolean
  /** Receives `balance_after` so the wallet card can refresh immediately. */
  onBalance?: (balance: number) => void
}

/** Maps the server problem type to the four distinct, user-safe messages. */
export function redeemErrorMessage(error: ApiError): string {
  const type = error.type
  if (type.endsWith('redemption_code_invalid')) return 'account.redeem.invalid'
  if (type.endsWith('redemption_code_used')) return 'account.redeem.used'
  if (type.endsWith('redemption_code_expired')) return 'account.redeem.expired'
  if (type.endsWith('redemption_code_disabled')) return 'account.redeem.disabled'
  if (error.kind === 'rate_limited') return 'account.redeem.rate_limited'
  return 'account.redeem.error'
}

/**
 * User-side redemption form (F-04-14). The server normalizes (trim → upper →
 * lookalike folding) before hashing, but the client still trims so a pasted
 * code with whitespace is not sent as a different value.
 */
export function RedeemPage({ enabled = true, onBalance }: RedeemPageProps) {
  const { t } = useI18n()
  const push = useToastStore((state) => state.push)
  const [code, setCode] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const [errorKey, setErrorKey] = useState<string | null>(null)

  if (!enabled) return null

  async function handleSubmit(event: FormEvent<HTMLFormElement>): Promise<void> {
    event.preventDefault()
    const trimmed = code.trim()
    if (trimmed === '' || submitting) return

    setSubmitting(true)
    setErrorKey(null)
    try {
      const data = requireData(
        await apiClient.POST('/api/redemption-codes/redeem', {
          body: { code: trimmed },
        }),
      )
      push({
        tone: 'success',
        message: t('account.redeem.success', {
          credits: formatCredits(data.credits),
        }),
      })
      setCode('')
      onBalance?.(data.balance_after)
    } catch (caught) {
      // Keep the typed value so the user can correct it; distinguish the four
      // reasons instead of collapsing them into one opaque failure.
      setErrorKey(
        caught instanceof ApiError
          ? redeemErrorMessage(caught)
          : 'account.redeem.error',
      )
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <section className="rounded-xl border border-border-hairline bg-surface-card p-5">
      <h2 className="text-card-title text-ink-1">
        {t('account.redeem.title')}
      </h2>
      <form className="mt-3" onSubmit={(event) => void handleSubmit(event)}>
        <div className="flex items-start gap-2">
          <div className="flex-1">
            <TextField
              label={t('account.redeem.input_label')}
              placeholder={t('account.redeem.placeholder')}
              maxLength={64}
              autoComplete="off"
              value={code}
              error={errorKey === null ? undefined : t(errorKey)}
              onChange={(event) => {
                setCode(event.target.value)
                if (errorKey !== null) setErrorKey(null)
              }}
            />
          </div>
          <Button
            type="submit"
            className="mt-[26px]"
            disabled={submitting || code.trim() === ''}
          >
            {submitting ? t('account.redeem.redeeming') : t('account.redeem.button')}
          </Button>
        </div>
      </form>
    </section>
  )
}
