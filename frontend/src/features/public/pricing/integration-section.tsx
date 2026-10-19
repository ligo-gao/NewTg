/**
 * 详情页「接入」区块（F-09-27）：三 Tab 代码、密钥提示、按分组限速、
 * 认证头说明与参数表。Tab 使用 WAI-ARIA tablist + roving focus。
 */

import { useId, useState } from 'react'

import type { PublicModelDetail } from './api'
import { sampleCode, sampleTarget, type SampleTab } from './code-samples'
import { copyText } from './copy'
import { handleTabListKeyDown } from '@/shared/lib/roving-focus'
import { useI18n } from '@/shared/lib/i18n-context'
import { useToastStore } from '@/shared/ui/toast'

const TABS: readonly SampleTab[] = ['openai', 'anthropic', 'gemini']

export interface IntegrationSectionProps {
  detail: PublicModelDetail
  serverAddress: string | undefined
  isLoggedIn: boolean
}

export function IntegrationSection({
  detail,
  serverAddress,
  isLoggedIn,
}: IntegrationSectionProps) {
  const { t, locale } = useI18n()
  const [tab, setTab] = useState<SampleTab>('openai')
  const panelId = useId()
  const push = useToastStore((state) => state.push)
  const target = sampleTarget(serverAddress)
  const code = sampleCode(tab, detail.model_id, target)

  const copy = async (text: string) => {
    const ok = await copyText(text)
    push({
      tone: ok ? 'success' : 'error',
      message: ok ? t('public.pricing.copied') : t('public.pricing.copy_failed'),
    })
  }

  return (
    <section aria-labelledby={`${panelId}-title`} className="flex flex-col gap-4">
      <h2 id={`${panelId}-title`} className="text-base font-semibold leading-6 text-ink-1">
        {t('public.pricing.detail.integration')}
      </h2>

      <div role="tablist" aria-label={t('public.pricing.detail.integration')} onKeyDown={handleTabListKeyDown} className="inline-flex items-center gap-1 self-start rounded-lg border border-border-hairline bg-surface-subtle p-1">
        {TABS.map((option) => {
          const selected = option === tab
          return (
            <button
              key={option}
              type="button"
              role="tab"
              id={`${panelId}-${option}`}
              aria-selected={selected}
              aria-controls={`${panelId}-panel`}
              tabIndex={selected ? 0 : -1}
              onClick={() => setTab(option)}
              className={[
                'rounded-md px-3 py-1 text-[13px] font-medium leading-5',
                selected ? 'bg-surface-card text-ink-1 shadow-sm' : 'text-ink-2 hover:text-ink-1',
                'focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none',
              ].join(' ')}
            >
              {option === 'openai'
                ? 'OpenAI'
                : option === 'anthropic'
                  ? 'Anthropic'
                  : 'Gemini'}
            </button>
          )
        })}
      </div>

      <div role="tabpanel" id={`${panelId}-panel`} aria-labelledby={`${panelId}-${tab}`} className="relative rounded-lg border border-border-default bg-surface-card">
        <button
          type="button"
          aria-label={t('public.pricing.detail.copy_code')}
          onClick={() => void copy(code)}
          className="absolute right-2 top-2 inline-flex size-7 items-center justify-center rounded text-ink-3 hover:bg-surface-hover hover:text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
        >
          <svg aria-hidden="true" viewBox="0 0 16 16" className="size-4" fill="none" stroke="currentColor" strokeWidth="1.5">
            <rect x="5" y="5" width="8" height="8" rx="1.5" />
            <path d="M11 5V4a1 1 0 0 0-1-1H4a1 1 0 0 0-1 1v6a1 1 0 0 0 1 1h1" />
          </svg>
        </button>
        <pre tabIndex={0} className="overflow-x-auto p-4 pr-12 text-[13px] leading-5 text-ink-1">
          <code>{code}</code>
        </pre>
        {target.configured ? null : (
          <p className="border-t border-warning-line bg-warning-bg px-4 py-2 text-xs text-warning">
            {t('public.pricing.detail.server_address_unset')}
          </p>
        )}
      </div>

      <div className="flex flex-wrap items-center gap-2 rounded-lg border border-info-line bg-info-bg px-3 py-2 text-[13px] text-info">
        <span>{t('public.pricing.detail.key_hint')}</span>
        <a
          href={isLoggedIn ? '/tokens' : '/sign-in?redirect=%2Ftokens'}
          className="font-medium underline focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
        >
          {isLoggedIn
            ? t('public.pricing.detail.create_token')
            : t('public.pricing.detail.sign_in_token')}
        </a>
      </div>

      {detail.rate_limits.length > 0 ? (
        <div className="flex flex-col gap-2">
          <h3 className="text-[13px] font-semibold text-ink-1">
            {t('public.pricing.detail.rate_limits')}
          </h3>
          <div className="overflow-x-auto rounded-lg border border-border-default">
            <table className="w-full min-w-[420px] text-[13px]">
              <thead className="bg-surface-subtle text-left text-xs text-ink-2">
                <tr className="h-9">
                  <th scope="col" className="px-3">{t('public.pricing.detail.group')}</th>
                  <th scope="col" className="px-3 text-right">{t('public.pricing.detail.rpm')}</th>
                  <th scope="col" className="px-3 text-right">{t('public.pricing.detail.effective')}</th>
                </tr>
              </thead>
              <tbody>
                {detail.rate_limits.map((limit) => (
                  <tr key={limit.group} className="h-11 border-t border-border-hairline">
                    <td className="px-3 text-ink-1">{limit.group}</td>
                    <td className="px-3 text-right font-mono tabular-nums text-ink-1">
                      {limit.user_model_rpm === 0
                        ? t('public.pricing.detail.unlimited')
                        : new Intl.NumberFormat(locale).format(limit.user_model_rpm)}
                    </td>
                    <td className="px-3 text-right text-ink-2">
                      {limit.user_model_rpm === 0
                        ? t('public.pricing.detail.unlimited')
                        : new Intl.NumberFormat(locale).format(limit.user_model_rpm)}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      ) : null}

      <div className="flex flex-col gap-2">
        <h3 className="text-[13px] font-semibold text-ink-1">
          {t('public.pricing.detail.auth')}
        </h3>
        <pre className="overflow-x-auto rounded-lg border border-border-default bg-surface-card p-4 text-[13px] leading-5 text-ink-1">
          <code>{`Authorization: Bearer <YOUR_TOKEN>
x-api-key: <YOUR_TOKEN>
anthropic-version: 2023-06-01`}</code>
        </pre>
      </div>

      {detail.parameters.length > 0 ? (
        <div className="flex flex-col gap-2">
          <h3 className="text-[13px] font-semibold text-ink-1">
            {t('public.pricing.detail.parameters')}
          </h3>
          <div className="overflow-x-auto rounded-lg border border-border-default">
            <table className="w-full min-w-[640px] text-[13px]">
              <thead className="bg-surface-subtle text-left text-xs text-ink-2">
                <tr className="h-9">
                  <th scope="col" className="px-3">{t('public.pricing.detail.param_name')}</th>
                  <th scope="col" className="px-3">{t('public.pricing.detail.param_required')}</th>
                  <th scope="col" className="px-3">{t('public.pricing.detail.param_type')}</th>
                  <th scope="col" className="px-3">{t('public.pricing.detail.param_default')}</th>
                  <th scope="col" className="px-3">{t('public.pricing.detail.param_description')}</th>
                </tr>
              </thead>
              <tbody>
                {detail.parameters.map((parameter) => (
                  <tr key={parameter.name} className="h-11 border-t border-border-hairline">
                    <td className="px-3 font-mono text-ink-1">{parameter.name}</td>
                    <td className="px-3 text-ink-2">
                      {parameter.required
                        ? t('public.pricing.detail.required')
                        : t('public.pricing.detail.optional')}
                    </td>
                    <td className="px-3 text-ink-2">{parameter.type}</td>
                    <td className="px-3 font-mono text-ink-2">
                      {parameter.default === null || parameter.default === undefined
                        ? '—'
                        : String(parameter.default)}
                    </td>
                    <td className="px-3 text-ink-2">{parameter.description}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      ) : null}
    </section>
  )
}
