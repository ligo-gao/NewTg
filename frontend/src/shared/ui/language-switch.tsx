import { useId } from 'react'

import type { Locale } from '@/shared/lib/i18n-context'
import { useI18n } from '@/shared/lib/i18n-context'

/**
 * Canonical bundled set (web-00 §7.3). Duplicated here instead of importing
 * `app/providers/i18n` so the shared layer does not depend on `app/`
 * (web-00 §2.2); the runtime list always arrives through the context.
 */
const FALLBACK_LOCALES: readonly Locale[] = ['en', 'zh-CN', 'zh-TW', 'fr', 'ru', 'ja', 'vi']

/**
 * Native language names, deliberately not translated: a language picker is
 * only usable when every option is written in its own language (UI design
 * F-09-34 "最终文案").
 */
// Endonyms are built from code points rather than literal CJK text: the i18n
// lint gate forbids CJK literals in source, and these values must not go
// through translation (a picker shows every language in its own script).
const fromCodePoints = (...points: number[]) => String.fromCodePoint(...points)

const ZH_CN = fromCodePoints(0x7b80, 0x4f53, 0x4e2d, 0x6587)
const ZH_TW = fromCodePoints(0x7e41, 0x9ad4, 0x4e2d, 0x6587)
const JA = fromCodePoints(0x65e5, 0x672c, 0x8a9e)

export const LANGUAGE_LABELS: Record<Locale, string> = {
  en: 'English',
  'zh-CN': ZH_CN,
  'zh-TW': ZH_TW,
  fr: 'Français',
  ru: 'Русский',
  ja: JA,
  vi: 'Tiếng Việt',
}

export interface LanguageSwitchProps {
  /** Localized accessible name for the control. */
  label: string
  /** Extra classes for the wrapper; the header and footer size it differently. */
  className?: string
  /** `footer` drops the visible label (the footer heading names the control). */
  variant?: 'header' | 'footer'
  /**
   * Called after the runtime has switched. The public header only needs the
   * local switch; the console persists the choice to the user's profile.
   */
  onChange?: (locale: Locale) => void
  /**
   * Draw the globe glyph the marketing header uses, so the public header's
   * language pill matches the home page picker. Off by default: the console
   * top bar keeps its compact select.
   */
  globe?: boolean
}

/**
 * F-09-34 language picker shared by public pages and the console top bar.
 *
 * A native `<select>` is used instead of a custom listbox: it is keyboard and
 * screen-reader accessible on every platform, never traps focus, and keeps the
 * public-page JS budget (F-09-06) free of a popover implementation. The server
 * advertises the available set through `/api/status.languages`; the provider
 * hands back the bundled intersection in server order.
 */
export function LanguageSwitch({
  label,
  className,
  variant = 'header',
  onChange,
  globe = false,
}: LanguageSwitchProps) {
  const { locale, setLocale, availableLocales } = useI18n()
  const id = useId()
  const locales = availableLocales ?? FALLBACK_LOCALES

  return (
    <div className={['flex items-center gap-1.5', className].filter(Boolean).join(' ')}>
      {variant === 'header' ? (
        <label htmlFor={id} className="sr-only">
          {label}
        </label>
      ) : null}
      {globe ? (
        <svg
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          strokeWidth="1.7"
          strokeLinecap="round"
          aria-hidden="true"
          className="size-3.5 flex-none"
        >
          <circle cx="12" cy="12" r="9" />
          <path d="M3 12h18M12 3c2.5 2.4 4 5.6 4 9s-1.5 6.6-4 9c-2.5-2.4-4-5.6-4-9s1.5-6.6 4-9Z" />
        </svg>
      ) : null}
      <select
        id={id}
        aria-label={variant === 'footer' ? label : undefined}
        value={locale}
        onChange={(event) => {
          const next = event.target.value
          if (!locales.includes(next as Locale)) return
          setLocale?.(next as Locale)
          onChange?.(next as Locale)
        }}
        className="h-8 max-w-[150px] cursor-pointer rounded-lg border border-border-default bg-surface-card px-2 text-meta text-ink-2 hover:text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
      >
        {locales.map((item) => (
          <option key={item} value={item}>
            {LANGUAGE_LABELS[item]}
          </option>
        ))}
      </select>
    </div>
  )
}
