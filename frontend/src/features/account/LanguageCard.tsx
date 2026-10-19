import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'

import {
  selfSettingsQueryOptions,
  SELF_SETTINGS_QUERY_KEY,
  updateLanguagePreference,
} from '@/lib/self-settings'
import { useI18n } from '@/shared/lib/i18n-context'
import type { Locale } from '@/shared/lib/i18n-context'
import { useToastStore } from '@/shared/ui/toast'

/** 语言名用各自的本族名显示（F-01-19 下拉），因此不随当前界面语言变化。 */
function languageNameKey(locale: Locale): string {
  return `account.language.name_${locale.toLowerCase().replace(/-/g, '_')}`
}

/**
 * F-01-19 界面语言卡片。
 *
 * 切换即时生效（先更新运行时），随后写入服务端个人偏好：
 *   - 已登录时走 PUT /api/self/setting（乐观锁，必须带 GET /api/self 的
 *     updated_at）；失败则回退到原语言并 Toast（modules-01 异常流程）。
 *   - 服务端偏好不可用（如接口故障）时只保留本地记忆，界面仍可用。
 *
 * 服务端返回的新设置写回同一 QueryClient 缓存，`LocalePreferenceSync`
 * 因此不会用旧值把刚切换的语言又覆盖回去。
 */
export function LanguageCard() {
  const { t, locale, availableLocales, setLocale } = useI18n()
  const queryClient = useQueryClient()
  const push = useToastStore((state) => state.push)
  const [pending, setPending] = useState(false)

  const selfQuery = useQuery(selfSettingsQueryOptions())
  const locales =
    availableLocales !== undefined && availableLocales.length > 0
      ? availableLocales
      : [locale]

  const save = useMutation({
    mutationFn: async (next: Locale) => {
      const current = selfQuery.data
      if (current === undefined) return null
      const updated = await updateLanguagePreference(next, current.updated_at)
      queryClient.setQueryData(SELF_SETTINGS_QUERY_KEY, updated)
      return updated
    },
  })

  async function handleChange(next: Locale): Promise<void> {
    const previous = locale
    setLocale?.(next)
    setPending(true)
    try {
      await save.mutateAsync(next)
      push({ tone: 'success', message: t('account.language.updated') })
    } catch {
      // 服务端写入失败：回退显示原语言，避免本地与服务端长期分叉。
      setLocale?.(previous)
      push({ tone: 'error', message: t('account.language.update_error') })
    } finally {
      setPending(false)
    }
  }

  return (
    <section className="rounded-xl border border-border-hairline bg-surface-card p-5">
      <h2 className="text-base font-semibold text-ink-1">
        {t('account.language.title')}
      </h2>
      <p className="mt-1 text-[13px] text-ink-2">
        {t('account.language.description')}
      </p>
      <label className="mt-4 flex max-w-[320px] flex-col gap-1.5 text-[13px] text-ink-2">
        <span>{t('account.language.label')}</span>
        <select
          value={locale}
          disabled={pending}
          onChange={(event) => {
            void handleChange(event.target.value as Locale)
          }}
          className="h-9 rounded-lg border border-border-default bg-surface-page px-2 text-sm text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none disabled:opacity-60"
        >
          {locales.map((item) => (
            <option key={item} value={item}>
              {t(languageNameKey(item))}
            </option>
          ))}
        </select>
      </label>
    </section>
  )
}
