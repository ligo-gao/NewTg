import { createFileRoute } from '@tanstack/react-router'

import { loadRouteDictionary } from '@/app/providers/i18n'
import { Breadcrumb } from '@/features/public/breadcrumb'
import { ModelPerfPage } from '@/features/public/model-perf-page'
import { PublicLayout } from '@/features/public/public-layout'
import { useI18n } from '@/shared/lib/i18n-context'

export const Route = createFileRoute('/pricing/$modelId')({
  loader: () => loadRouteDictionary('public'),
  component: ModelPerfRoute,
})

/**
 * F-09-25 的公开承载路由（modules-09 §3.12 将性能区放在模型详情页）。
 * M6b 只交付性能区；定价广场与详情信息区属于 M7，因此面包屑中间层按设计
 * 以「未开放」禁用态渲染，而不是链到一个尚不存在的 `/pricing`。
 */
function ModelPerfRoute() {
  const { modelId } = Route.useParams()
  const { t } = useI18n()

  return (
    <PublicLayout activePath={`/pricing/${modelId}`}>
      <Breadcrumb
        items={[
          { label: t('public.nav.home'), path: '/' },
          {
            label: t('public.nav.pricing'),
            disabled: true,
            disabledHint: t('public.breadcrumb.not_open'),
          },
          { label: modelId, current: true },
        ]}
      />
      <div className="mt-6">
        <ModelPerfPage modelId={modelId} />
      </div>
    </PublicLayout>
  )
}
