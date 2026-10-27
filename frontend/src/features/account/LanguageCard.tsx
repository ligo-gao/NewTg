import { useState } from 'react'

import { useLanguageSelection } from '@/lib/use-language-selection'
import { useI18n } from '@/shared/lib/i18n-context'
import type { Locale } from '@/shared/lib/i18n-context'

/** 语言名用各自的本族名显示（F-01-19 下拉），因此不随当前界面语言变化。 */
function languageNameKey(locale: Locale): string {
  return `account.language.name_${locale.toLowerCase().replace(/-/g, '_')}`
}

/**
 * F-01-19 界面语言卡片。
 *
 * 切换即时生效（先更新运行时），随后由共享的 `useLanguageSelection` 写入
 * 服务端个人偏好。服务端写入失败**不回滚**界面语言：本地选择与 `pending`
 * 标记都保留，提示「已保存在本机」，下次启动再补写（设计 §4.7）。
 *
 * `pending` 只用于保存期间禁用下拉，避免连点产生并发的乐观锁冲突。
 */
export function LanguageCard() {
  const { t, locale, availableLocales, setLocale } = useI18n()
  const { onChange } = useLanguageSelection(true)
  const [pending, setPending] = useState(false)

  const locales =
    availableLocales !== undefined && availableLocales.length > 0
      ? availableLocales
      : [locale]

  async function handleChange(next: Locale): Promise<void> {
    setLocale?.(next)
    setPending(true)
    try {
      await onChange?.(next)
    } finally {
      setPending(false)
    }
  }

  return (
    <section className="rounded-xl border border-border-hairline bg-surface-card p-5">
      <h2 className="text-section text-ink-1">
        {t('account.language.title')}
      </h2>
      <p className="mt-1 text-meta text-ink-2">
        {t('account.language.description')}
      </p>
      <label className="mt-4 flex max-w-[320px] flex-col gap-1.5 text-meta text-ink-2">
        <span>{t('account.language.label')}</span>
        <select
          value={locale}
          disabled={pending}
          onChange={(event) => {
            void handleChange(event.target.value as Locale)
          }}
          className="h-9 rounded-lg border border-border-default bg-surface-page px-2 text-body text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none disabled:opacity-60"
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
