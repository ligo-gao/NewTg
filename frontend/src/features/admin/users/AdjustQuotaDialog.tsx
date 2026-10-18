import { useEffect, useState, type FormEvent } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'

import { apiClient } from '@/api/client'
import { requireData } from '@/api/result'
import type { components } from '@/api/schema'
import { formatCredits } from '@/shared/lib/format'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { Dialog } from '@/shared/ui/base/dialog'
import { FormAlert, TextField } from '@/shared/ui/form'
import { useToastStore } from '@/shared/ui/toast'

type AdminUser = components['schemas']['AdminUser']
type QuotaMode = 'add' | 'subtract' | 'override'

export interface AdjustQuotaDialogProps {
  user: AdminUser
  open: boolean
  onOpenChange: (open: boolean) => void
  onAdjusted?: (newBalance: number) => void
}

const MODES: QuotaMode[] = ['add', 'subtract', 'override']

function parseAmount(value: string): number | undefined {
  if (value.trim() === '') return undefined
  const amount = Number(value)
  return Number.isInteger(amount) && amount >= 0 ? amount : undefined
}

/**
 * F-07-11 quota adjustment. Subtract and override always pass through a
 * second confirmation (override=0 included, since it can wipe the balance);
 * add skips it. The business idempotency key is generated per submission.
 */
export function AdjustQuotaDialog({
  user,
  open,
  onOpenChange,
  onAdjusted,
}: AdjustQuotaDialogProps) {
  const { t } = useI18n()
  const queryClient = useQueryClient()
  const push = useToastStore((state) => state.push)
  const [mode, setMode] = useState<QuotaMode>('add')
  const [amount, setAmount] = useState('')
  const [stage, setStage] = useState<'form' | 'confirm'>('form')
  const [error, setError] = useState<string | undefined>(undefined)
  const [submitting, setSubmitting] = useState(false)

  useEffect(() => {
    if (!open) return
    setMode('add')
    setAmount('')
    setStage('form')
    setError(undefined)
  }, [open])

  // Balance changes made elsewhere (another admin, a relay settlement) must
  // not be overwritten from a stale list row: refetch the authoritative user
  // when the dialog opens.
  const detailQuery = useQuery({
    queryKey: ['admin-user', user.id],
    queryFn: async () =>
      requireData(
        await apiClient.GET('/api/users/{id}', { params: { path: { id: user.id } } }),
      ),
    enabled: open,
    staleTime: 0,
  })
  const currentBalance = detailQuery.data?.balance ?? user.balance

  const parsed = parseAmount(amount)
  const exceeds = mode === 'subtract' && parsed !== undefined && parsed > currentBalance
  const noChange = mode === 'override' && parsed !== undefined && parsed === currentBalance
  // Add/subtract move the balance by at least one credit; override may set 0.
  const belowMinimum = mode !== 'override' && parsed !== undefined && parsed < 1
  const valid = parsed !== undefined && !exceeds && !noChange && !belowMinimum
  const result =
    parsed === undefined
      ? undefined
      : mode === 'add'
        ? currentBalance + parsed
        : mode === 'subtract'
          ? currentBalance - parsed
          : parsed

  async function submit(): Promise<void> {
    if (parsed === undefined) return
    setError(undefined)
    setSubmitting(true)
    try {
      const data = requireData(
        await apiClient.POST('/api/users/{id}/quota-adjustment', {
          params: { path: { id: user.id } },
          body: {
            mode,
            amount: parsed,
            // Distinct from the HTTP Idempotency-Key: this one is persisted in
            // the transaction ledger (contract freeze).
            idempotency_key: crypto.randomUUID(),
          },
        }),
      )
      push({ tone: 'success', message: t('admin.quota.success') })
      onAdjusted?.(data.new_balance)
      await queryClient.invalidateQueries({ queryKey: ['admin-users'] })
      await queryClient.invalidateQueries({ queryKey: ['admin-user', user.id] })
      onOpenChange(false)
    } catch {
      setError(t('admin.quota.error'))
    } finally {
      setSubmitting(false)
    }
  }

  function handleSubmit(event: FormEvent<HTMLFormElement>): void {
    event.preventDefault()
    if (!valid) return
    if (mode === 'add') {
      void submit()
      return
    }
    setStage('confirm')
  }

  const confirmBody =
    mode === 'subtract'
      ? t('admin.quota.confirm_subtract', {
          username: user.username,
          amount: formattedAmount(parsed),
        })
      : t('admin.quota.confirm_override', {
          username: user.username,
          amount: formattedAmount(parsed),
        })

  return (
    <Dialog
      open={open}
      onOpenChange={onOpenChange}
      title={stage === 'form' ? t('admin.quota.title') : t('admin.quota.confirm_title')}
      description={stage === 'form' ? t('admin.quota.user', { username: user.username }) : undefined}
      footer={
        stage === 'form' ? (
          <>
            <Button variant="secondary" onClick={() => onOpenChange(false)}>
              {t('common.action.cancel')}
            </Button>
            <Button type="submit" form="quota-form" disabled={!valid || submitting}>
              {t('admin.quota.submit')}
            </Button>
          </>
        ) : (
          <>
            <Button variant="secondary" onClick={() => setStage('form')}>
              {t('common.action.cancel')}
            </Button>
            <Button
              variant="danger"
              disabled={submitting}
              onClick={() => void submit()}
            >
              {t('admin.quota.submit')}
            </Button>
          </>
        )
      }
    >
      {stage === 'form' ? (
        <form id="quota-form" className="flex flex-col gap-3" onSubmit={handleSubmit}>
          {error !== undefined ? <FormAlert>{error}</FormAlert> : null}
          <p className="font-mono text-[13px] tabular-nums text-ink-2">
            {t('admin.quota.current', {
              balance: formatCredits(currentBalance),
            })}
          </p>

          <div role="radiogroup" aria-label={t('admin.quota.mode')} className="grid grid-cols-3 gap-1">
            {MODES.map((value) => (
              <button
                key={value}
                type="button"
                role="radio"
                aria-checked={mode === value}
                className={[
                  'h-9 rounded-lg border text-[13px] font-medium focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none',
                  mode === value
                    ? 'border-brand bg-brand-subtle text-brand'
                    : 'border-border-default bg-surface-card text-ink-2 hover:bg-surface-hover',
                ].join(' ')}
                onClick={() => {
                  setMode(value)
                }}
              >
                {value === 'add'
                  ? t('admin.quota.mode_add')
                  : value === 'subtract'
                    ? t('admin.quota.mode_subtract')
                    : t('admin.quota.mode_override')}
              </button>
            ))}
          </div>

          <TextField
            label={t('admin.quota.amount')}
            inputMode="numeric"
            value={amount}
            onChange={(event) => {
              setAmount(event.target.value.replace(/\D/g, ''))
            }}
            error={
              exceeds
                ? t('admin.quota.exceeds')
                : noChange
                  ? t('admin.quota.unchanged')
                  : belowMinimum
                    ? t('admin.quota.minimum')
                    : undefined
            }
          />

          <div className="rounded-lg bg-surface-subtle px-3 py-2 text-[13px] text-ink-2" aria-live="polite">
            {result === undefined
              ? t('admin.quota.preview_placeholder')
              : mode === 'add'
                ? t('admin.quota.preview_add', {
                    current: formatCredits(currentBalance),
                    amount: formatCredits(parsed ?? 0),
                    result: formatCredits(result),
                  })
                : mode === 'subtract'
                  ? t('admin.quota.preview_subtract', {
                      current: formatCredits(currentBalance),
                      amount: formatCredits(parsed ?? 0),
                      result: formatCredits(Math.max(result, 0)),
                    })
                  : t('admin.quota.preview_override', {
                      result: formatCredits(result),
                    })}
          </div>
          {mode === 'override' && parsed === 0 ? (
            <p className="text-xs text-warning">{t('admin.quota.override_zero')}</p>
          ) : null}
        </form>
      ) : (
        <p className="text-sm text-ink-2">{confirmBody}</p>
      )}
    </Dialog>
  )
}

function formattedAmount(amount: number | undefined): string {
  return amount === undefined ? '0' : formatCredits(amount)
}
