import { createFileRoute } from '@tanstack/react-router'

import { loadRouteDictionary } from '@/app/providers/i18n'
import { ModelConfigPage } from '@/features/model-config/model-config-page'

/**
 * `/model-config`（F-02-18）。
 *
 * 唯一可分享的页面状态是 `?focus=default-model`：Playground 页头的
 * 「默认模型」链接用它把焦点送到默认模型卡。其余状态留在组件内部。
 */
export interface ModelConfigSearch {
  focus?: 'default-model'
}

export const Route = createFileRoute('/_console/model-config')({
  validateSearch: (search: Record<string, unknown>): ModelConfigSearch => {
    return search.focus === 'default-model'
      ? { focus: 'default-model' }
      : {}
  },
  loader: () => loadRouteDictionary('account'),
  component: ModelConfigRoute,
})

function ModelConfigRoute() {
  const search = Route.useSearch()
  return <ModelConfigPage focusDefaultModel={search.focus === 'default-model'} />
}
