import { useQuery } from '@tanstack/react-query'

import { apiClient } from '@/api/client'
import { requireData } from '@/api/result'
import { useI18n } from '@/shared/lib/i18n-context'

import { isUiSupportedProvider } from '../topup/types'
import { EMPTY_PROVIDERS, TOPUP_CONFIG_KEY } from './constants'
import type { SubscriptionChannelProvider } from './types'

/**
 * 已启用且本构建能端到端渲染的渠道。多个调用方共用同一查询键，React Query
 * 只发一次请求；`isPending` 用于避免在配置到达前把「暂无渠道」误报给用户。
 *
 * `enabled` 让「我的订阅」在没有打开任何写弹窗时保持静默：续费/变更弹窗
 * 与列表常驻挂载，若默认查询就会让钱包页一挂载即请求 `/api/topup/config`。
 */
export function useEnabledChannelProviders(enabled = true): {
  providers: readonly SubscriptionChannelProvider[]
  isPending: boolean
  /**
   * Stripe publishable key from the same public config. Checkout dialogs need
   * it to mount Elements; reading it here keeps renew/change on the exact same
   * cached query as the picker instead of issuing a second `GET /api/topup/config`.
   */
  publishableKey?: string
} {
  const query = useQuery({
    queryKey: TOPUP_CONFIG_KEY,
    queryFn: async () => requireData(await apiClient.GET('/api/topup/config')),
    enabled,
  })
  return {
    providers: (query.data?.enabled_providers ?? EMPTY_PROVIDERS).filter(
      isUiSupportedProvider,
    ),
    isPending: query.isPending,
    publishableKey: query.data?.providers.stripe?.publishable_key,
  }
}

/**
 * 续费/升降级共用的渠道选择器。
 *
 * 渠道来源是 `GET /api/topup/config.enabled_providers`（服务端未配置的渠道
 * 会以 503 拒绝），并复用 `isUiSupportedProvider` 过滤掉尚未接好收银台的
 * epay（G-DOM-72 ④）。wallet 不在此列：续费/升降级的契约只接受
 * stripe/alipay/epay（见 types.ts 的 SubscriptionChannelProvider 注释）。
 */
export function ChannelProviderPicker({
  name,
  value,
  onChange,
  disabled,
}: {
  name: string
  value: SubscriptionChannelProvider | null
  onChange: (provider: SubscriptionChannelProvider) => void
  disabled: boolean
}) {
  const { t } = useI18n()
  const { providers, isPending } = useEnabledChannelProviders()

  return (
    <fieldset>
      <legend className="mb-2 text-[13px] font-medium text-ink-1">
        {t('account.subscription.purchase.payment_method')}
      </legend>
      {providers.length === 0 ? (
        <p className="text-sm text-ink-2">
          {isPending
            ? t('account.subscription.purchase.loading_providers')
            : t('account.subscription.purchase.no_providers')}
        </p>
      ) : (
        <div className="flex flex-col gap-2">
          {providers.map((option) => (
            <label
              key={option}
              className="flex cursor-pointer items-center gap-2 rounded-lg border border-border-default bg-surface-page px-3 py-2 text-sm text-ink-1"
            >
              <input
                type="radio"
                name={name}
                value={option}
                checked={value === option}
                disabled={disabled}
                onChange={() => onChange(option)}
                className="accent-brand"
              />
              {t(
                option === 'stripe'
                  ? 'account.orders.provider_stripe'
                  : 'account.orders.provider_alipay',
              )}
            </label>
          ))}
        </div>
      )}
    </fieldset>
  )
}
