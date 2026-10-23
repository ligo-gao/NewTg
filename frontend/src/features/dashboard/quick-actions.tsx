import type { JSX } from 'react'

import { usePlaygroundEnabled } from '@/features/playground/visibility'
import { NavIcon, type NavIconKey } from '@/features/shell/nav-icon'
import { useI18n } from '@/shared/lib/i18n-context'

interface QuickAction {
  href: string
  icon: NavIconKey
  testId: string
  title: string
  hint: string
}

/**
 * Dashboard quick actions (spec §7).
 *
 * Four fixed entry points replace the "go back to the sidebar and hunt" loop.
 * The Playground slot follows the same public-status flag the sidebar uses
 * (`usePlaygroundEnabled`), so a site that hides the module never shows a dead
 * link. Links are plain anchors: they keep middle-click and keyboard
 * behaviour, and the dashboard is not a router-owned list.
 */
export function QuickActions(): JSX.Element {
  const { t } = useI18n()
  const playgroundEnabled = usePlaygroundEnabled()

  const actions: QuickAction[] = [
    {
      href: '/tokens',
      icon: 'key',
      testId: 'quick-action-create-token',
      title: t('common.dashboard.quick.create_token'),
      hint: t('common.dashboard.quick.create_token_hint'),
    },
    {
      href: '/wallet',
      icon: 'wallet',
      testId: 'quick-action-topup',
      title: t('common.dashboard.quick.topup'),
      hint: t('common.dashboard.quick.topup_hint'),
    },
    {
      href: '/dashboard/usage-logs',
      icon: 'list',
      testId: 'quick-action-logs',
      title: t('common.dashboard.quick.logs'),
      hint: t('common.dashboard.quick.logs_hint'),
    },
    {
      href: '/playground',
      icon: 'flask',
      testId: 'quick-action-playground',
      title: t('common.nav.playground'),
      hint: t('common.dashboard.quick.playground_hint'),
    },
  ]

  const visible = playgroundEnabled
    ? actions
    : actions.filter((action) => action.href !== '/playground')

  return (
    <section className="rounded-xl border border-border-hairline bg-surface-card p-4">
      <h3 className="text-sm font-semibold text-ink-1">
        {t('common.dashboard.quick.title')}
      </h3>
      <p className="mt-1 text-[13px] text-ink-2">
        {t('common.dashboard.quick.description')}
      </p>
      <div className="mt-3 grid grid-cols-2 gap-2.5">
        {visible.map((action) => (
          <a
            key={action.href}
            href={action.href}
            data-testid={action.testId}
            className="flex items-center gap-2.5 rounded-[10px] border border-border-hairline bg-surface-card px-3 py-2.5 text-ink-1 hover:border-brand-line hover:bg-brand-subtle focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
          >
            <span className="grid size-8 shrink-0 place-items-center rounded-lg bg-brand-subtle text-brand">
              <NavIcon name={action.icon} />
            </span>
            <span className="min-w-0">
              <span className="block text-[12.5px] leading-[17px] font-semibold">
                {action.title}
              </span>
              <span className="block text-[10.5px] leading-[15px] text-ink-2">
                {action.hint}
              </span>
            </span>
          </a>
        ))}
      </div>
    </section>
  )
}
