import { useQuery } from '@tanstack/react-query'

import {
  fetchModelPerf,
  type ModelPerfDimension,
  type ModelPerfRange,
} from './model-perf-api'

export interface ModelPerfParams {
  range: ModelPerfRange
  model?: string
  dimension?: ModelPerfDimension
}

/**
 * F-05-08 看板查询。查询键包含范围/模型/维度，保证切换窗口时不会把旧窗口
 * 的缓存当作新数据展示（指标口径偏差必须来自同一窗口）。
 */
export function useModelPerf(params: ModelPerfParams) {
  return useQuery({
    queryKey: [
      'model-perf',
      params.range,
      params.model ?? null,
      params.dimension ?? null,
    ],
    queryFn: ({ signal }) =>
      fetchModelPerf({
        range: params.range,
        model: params.model,
        dimension: params.dimension,
        signal,
      }),
    // 指标是分钟级聚合，窗口切换时保留上一份数据会误导「已刷新」的判断；
    // 旧数据只在离线恢复场景由 React Query 的 stale 语义保留。
    staleTime: 30_000,
  })
}
