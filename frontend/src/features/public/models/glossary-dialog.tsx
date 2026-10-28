/**
 * 数据口径（术语表）弹框。
 *
 * 九条说明全部复用既有的 `models.glossary.*` 文案，本组件不写第二套措辞：
 * 同一条口径在登记册、对比表与这里必须一字不差。
 */

import { useI18n } from '@/shared/lib/i18n-context'
import { Dialog } from '@/shared/ui/base/dialog'

export interface GlossaryDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
}

/** 顺序即阅读顺序：先指标，再样本/来源。 */
const GLOSSARY_KEYS = [
  'public.models.glossary.p50',
  'public.models.glossary.p95',
  'public.models.glossary.ttft',
  'public.models.glossary.tps',
  'public.models.glossary.success_rate',
  'public.models.glossary.sample',
  'public.models.glossary.source_test',
  'public.models.glossary.source_usage',
] as const

export function GlossaryDialog({ open, onOpenChange }: GlossaryDialogProps) {
  const { t } = useI18n()

  return (
    <Dialog
      open={open}
      onOpenChange={onOpenChange}
      title={t('public.models.glossary.title')}
      closeLabel={t('public.models.glossary.close')}
    >
      <ul className="grid gap-2.5">
        {GLOSSARY_KEYS.map((key) => (
          <li key={key} className="text-public-body text-ink-2">
            {t(key)}
          </li>
        ))}
      </ul>
    </Dialog>
  )
}
