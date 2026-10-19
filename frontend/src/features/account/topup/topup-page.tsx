import { useEffect, useState, type KeyboardEvent as ReactKeyboardEvent } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'

import { apiClient } from '@/api/client'
import { requireData } from '@/api/result'
import { useI18n } from '@/shared/lib/i18n-context'
import { formatCredits } from '@/shared/lib/format'
import { Button } from '@/shared/ui/button'
import { TextField } from '@/shared/ui/form'

import { BalanceCard } from './balance-card'
import { CheckoutDialog } from './checkout-dialog'
import { PaymentConfirmDialog } from './payment-confirm-dialog'
import { RedeemPage } from './redeem-page'
import { centsToCredits, formatYuan, parseYuanToCents } from './money'
import type {
  CreateOrderResponse,
  OrderView,
  TopupConfig,
  TopupProvider,
} from './types'

const CONFIG_KEY = ['topup', 'config'] as const
const WALLET_SUMMARY_KEY = ['wallet', 'summary'] as const

interface CheckoutState {
  response: CreateOrderResponse
  /** Replayed orders carry no payment payload; the full row is fetched. */
  replayOrder?: OrderView
}

/**
 * Recharge page (F-04-01/02/03/05, modules-04 §2).
 *
 * Selection state is local; the amount is always cents and the credits preview
 * is `cents × quota_per_unit / 100`. The server re-prices every order, so the
 * preview is never sent as a trusted value. The balance card is seeded from
 * `GET /api/wallet/summary` (F-03-01) and re-read after a successful top-up or
 * redemption — never incremented locally.
 */
export function TopupPage() {
  const { t } = useI18n()
  const queryClient = useQueryClient()

  const configQuery = useQuery({
    queryKey: CONFIG_KEY,
    queryFn: async () =>
      requireData(await apiClient.GET('/api/topup/config')),
  })
  const config: TopupConfig | undefined = configQuery.data

  const summaryQuery = useQuery({
    queryKey: WALLET_SUMMARY_KEY,
    queryFn: async () =>
      requireData(await apiClient.GET('/api/wallet/summary')),
  })
  const balance = summaryQuery.data?.balance ?? null

  const [amountCents, setAmountCents] = useState<number | null>(null)
  const [customMode, setCustomMode] = useState(false)
  const [customInput, setCustomInput] = useState('')
  const [provider, setProvider] = useState<TopupProvider | null>(null)
  const [confirmOpen, setConfirmOpen] = useState(false)
  const [checkout, setCheckout] = useState<CheckoutState | null>(null)

  // Seed selection from server config: first preset at/above the minimum, and
  // the first enabled provider (modules-04 §2.4).
  useEffect(() => {
    if (config === undefined) return
    setAmountCents((current) => {
      if (current !== null) return current
      return (
        config.amount_options.find((option) => option >= config.min_topup) ??
        config.amount_options[0] ??
        null
      )
    })
    setProvider((current) => current ?? config.enabled_providers[0] ?? null)
  }, [config])

  if (configQuery.isPending) {
    return <p className="text-sm text-ink-2">{t('account.topup.loading')}</p>
  }
  if (configQuery.isError || config === undefined) {
    return (
      <div className="flex items-center gap-3 text-sm text-ink-2">
        <span>{t('account.topup.config_error')}</span>
        <Button variant="secondary" onClick={() => void configQuery.refetch()}>
          {t('account.topup.retry')}
        </Button>
      </div>
    )
  }

  const noProviders = config.enabled_providers.length === 0
  const selectedCents = customMode
    ? parseYuanToCents(customInput)
    : amountCents

  const amountError = (() => {
    if (selectedCents === null) {
      // Empty custom input is not an error, just not payable yet.
      return customMode && customInput !== ''
        ? t('account.topup.min_error', { min: formatYuan(config.min_topup) })
        : undefined
    }
    if (selectedCents < config.min_topup) {
      return t('account.topup.min_error', { min: formatYuan(config.min_topup) })
    }
    if (selectedCents > config.max_topup) {
      return t('account.topup.max_error', { max: formatYuan(config.max_topup) })
    }
    return undefined
  })()

  const payable =
    !noProviders &&
    provider !== null &&
    selectedCents !== null &&
    amountError === undefined
  const credits =
    selectedCents === null ? 0 : centsToCredits(selectedCents, config.quota_per_unit)

  function handleCustomChange(value: string): void {
    // Positive integers only; non-digits never enter the controlled value.
    setCustomInput(value.replace(/\D/g, ''))
  }

  function handleCreated(response: CreateOrderResponse, replayOrder?: OrderView): void {
    setConfirmOpen(false)
    setCheckout({ response, replayOrder })
  }

  function refreshBalance(): void {
    void queryClient.invalidateQueries({ queryKey: WALLET_SUMMARY_KEY })
  }

  function handleSucceeded(): void {
    // The order endpoint is the payment truth; the balance is re-read from the
    // wallet summary instead of being incremented from credited_credits.
    refreshBalance()
    void queryClient.invalidateQueries({ queryKey: ['orders'] })
  }

  return (
    <div className="mx-auto flex max-w-[1080px] flex-col gap-4">
      <BalanceCard balance={balance} quotaPerUnit={config.quota_per_unit} />

      <section className="rounded-xl border border-border-hairline bg-surface-card p-5">
        <div className="flex items-center justify-between gap-3">
          <h1 className="text-[15px] font-semibold text-ink-1">
            {t('account.topup.title')}
          </h1>
          <a
            href="/orders"
            className="text-[13px] font-medium text-brand hover:text-brand-hover"
          >
            {t('account.topup.order_history')}
          </a>
        </div>

        {noProviders ? (
          <p className="mt-4 text-sm text-ink-2">{t('account.topup.no_providers')}</p>
        ) : (
          <>
            <fieldset className="mt-5">
              <legend className="mb-2 text-[13px] font-medium text-ink-1">
                {t('account.topup.amount_section')}
              </legend>
              <div
                role="radiogroup"
                aria-label={t('account.topup.amount_section')}
                className="flex flex-wrap gap-2"
                onKeyDown={moveRadioFocus}
              >
                {config.amount_options.map((option) => (
                  <AmountChip
                    key={option}
                    amount={option}
                    selected={!customMode && amountCents === option}
                    onSelect={() => {
                      setCustomMode(false)
                      setAmountCents(option)
                    }}
                  />
                ))}
                <button
                  type="button"
                  role="radio"
                  aria-checked={customMode}
                  onClick={() => setCustomMode(true)}
                  className={chipClass(customMode)}
                >
                  {t('account.topup.custom_amount')}
                </button>
              </div>
              {customMode ? (
                <div className="mt-3 max-w-[240px]">
                  <TextField
                    label={t('account.topup.custom_amount')}
                    placeholder={t('account.topup.custom_placeholder')}
                    inputMode="numeric"
                    autoComplete="off"
                    value={customInput}
                    error={amountError}
                    onChange={(event) => {
                      handleCustomChange(event.target.value)
                    }}
                  />
                </div>
              ) : amountError !== undefined ? (
                <p className="mt-2 text-xs text-danger">{amountError}</p>
              ) : null}
            </fieldset>

            <dl
              aria-live="polite"
              className="mt-5 flex flex-col gap-2 border-t border-dotted border-border-default pt-4"
            >
              <SummaryRow
                label={t('account.topup.you_pay')}
                value={formatYuan(selectedCents ?? 0)}
                testId="summary-pay"
                emphasize
              />
              <SummaryRow
                label={t('account.topup.credits_received')}
                value={formatCredits(credits)}
                testId="summary-credits"
              />
            </dl>

            <fieldset className="mt-5">
              <legend className="mb-2 text-[13px] font-medium text-ink-1">
                {t('account.topup.payment_method')}
              </legend>
              <div
                role="radiogroup"
                aria-label={t('account.topup.payment_method')}
                className="flex flex-col gap-2"
                onKeyDown={moveRadioFocus}
              >
                {config.enabled_providers.map((option) => (
                  <ProviderRadio
                    key={option}
                    provider={option}
                    minTopup={config.min_topup}
                    selected={provider === option}
                    onSelect={() => {
                      setProvider(option)
                    }}
                  />
                ))}
              </div>
            </fieldset>

            <Button
              className="mt-5 h-11 w-full"
              disabled={!payable}
              aria-label={t('account.topup.pay_aria', {
                amount: formatYuan(selectedCents ?? 0),
                credits: formatCredits(credits),
              })}
              onClick={() => {
                setConfirmOpen(true)
              }}
            >
              {t('account.topup.pay', {
                amount: formatYuan(selectedCents ?? 0),
              })}
            </Button>
          </>
        )}
      </section>

      <RedeemPage
        onBalance={() => {
          // Redemption already returns balance_after, but the wallet summary is
          // the shared source of truth with the rest of the console.
          refreshBalance()
        }}
      />

      {payable && provider !== null && selectedCents !== null ? (
        <PaymentConfirmDialog
          open={confirmOpen}
          amount={selectedCents}
          provider={provider}
          quotaPerUnit={config.quota_per_unit}
          onOpenChange={setConfirmOpen}
          onCreated={handleCreated}
        />
      ) : null}

      {checkout !== null ? (
        <CheckoutDialog
          open
          order={checkout.response.order}
          clientSecret={checkout.response.client_secret}
          qrCode={checkout.response.qr_code}
          replayOrder={checkout.replayOrder}
          publishableKey={config.providers.stripe?.publishable_key}
          balance={balance}
          onOpenChange={(next) => {
            if (!next) setCheckout(null)
          }}
          onSucceeded={handleSucceeded}
          onRefresh={() => {
            setCheckout(null)
            setConfirmOpen(true)
          }}
        />
      ) : null}
    </div>
  )
}

function chipClass(selected: boolean): string {
  return [
    'inline-flex h-10 items-center justify-center rounded-lg border px-4 text-sm font-medium transition-colors focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none',
    selected
      ? 'border-[1.5px] border-brand bg-brand-subtle text-brand'
      : 'border-border-default bg-surface-subtle text-ink-1 hover:bg-surface-hover',
  ].join(' ')
}

/**
 * Arrow-key navigation for the radiogroup pattern (§0.6): move focus to the
 * neighbouring radio and select it, mirroring native radio behaviour.
 */
function moveRadioFocus(event: ReactKeyboardEvent<HTMLDivElement>): void {
  const directions: Record<string, number> = {
    ArrowRight: 1,
    ArrowDown: 1,
    ArrowLeft: -1,
    ArrowUp: -1,
  }
  const step = directions[event.key]
  if (step === undefined) return
  const radios = Array.from(
    event.currentTarget.querySelectorAll<HTMLElement>('[role="radio"]'),
  )
  const index = radios.indexOf(event.target as HTMLElement)
  if (index === -1) return
  event.preventDefault()
  const next = radios[(index + step + radios.length) % radios.length]
  next?.focus()
  next?.click()
}

function AmountChip({
  amount,
  selected,
  onSelect,
}: {
  amount: number
  selected: boolean
  onSelect: () => void
}) {
  return (
    <button
      type="button"
      role="radio"
      aria-checked={selected}
      onClick={onSelect}
      className={chipClass(selected)}
    >
      {formatYuan(amount, { decimals: amount % 100 !== 0 })}
    </button>
  )
}

function ProviderRadio({
  provider,
  minTopup,
  selected,
  onSelect,
}: {
  provider: TopupProvider
  minTopup: number
  selected: boolean
  onSelect: () => void
}) {
  const { t } = useI18n()
  const label = t(
    provider === 'stripe'
      ? 'account.orders.provider_stripe'
      : 'account.orders.provider_alipay',
  )
  return (
    <button
      type="button"
      role="radio"
      aria-checked={selected}
      aria-label={label}
      onClick={onSelect}
      className={[
        'flex h-12 items-center justify-between rounded-lg border px-4 text-sm focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none',
        selected
          ? 'border-[1.5px] border-brand bg-brand-subtle'
          : 'border-border-default bg-surface-card hover:bg-surface-hover',
      ].join(' ')}
    >
      <span className="font-medium text-ink-1">{label}</span>
      <span className="text-xs text-ink-2">
        {t('account.topup.provider_min', { min: formatYuan(minTopup) })}
      </span>
    </button>
  )
}

function SummaryRow({
  label,
  value,
  testId,
  emphasize = false,
}: {
  label: string
  value: string
  testId?: string
  emphasize?: boolean
}) {
  return (
    <div className="flex items-end gap-2 text-sm">
      <dt className="text-ink-2">{label}</dt>
      <span
        aria-hidden="true"
        className="mb-1 flex-1 border-b border-dotted border-border-default"
      />
      <dd
        data-testid={testId}
        className={[
          'tabular-nums text-ink-1',
          emphasize ? 'text-[28px] font-semibold leading-9' : 'font-medium',
        ].join(' ')}
      >
        {value}
      </dd>
    </div>
  )
}
