/**
 * 冷启动时主区的「待累积」占位（浅色网格骨架 + 一句说明）。
 *
 * 这是 `sections.tsx` 剩下的唯一导出：完整的模型榜 / 升降榜 / 趋势 / 份额
 * 四个区块已由梯子 + 右栏取代，挪进 `ladder.tsx` / `rail.tsx`。
 */

import { useI18n } from '@/shared/lib/i18n-context'

export function SectionPlaceholder({ titleKey }: { titleKey: string }) {
  const { t } = useI18n()
  return (
    <section>
      <h2 className="text-public-section text-ink-1">
        {t(titleKey)}
      </h2>
      <div
        aria-hidden="true"
        className="mt-4 grid grid-cols-3 gap-2 opacity-40"
      >
        {Array.from({ length: 9 }, (_, index) => (
          <div key={index} className="h-8 rounded bg-surface-subtle" />
        ))}
      </div>
      <p className="mt-2 text-public-caption text-ink-2">
        {t('public.rankings.section.accumulating')}
      </p>
    </section>
  )
}
