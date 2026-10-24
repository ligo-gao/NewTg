/**
 * 模型库的「性能读数选源」（modules-09 F-09-25）。
 *
 * 读数来源必须诚实——这是模型库最重要的展示纪律：
 * 1. 有管理员主动测速（`latest_test`）时，用它的 P50/P95；
 * 2. 没有测速记录时才回退到真实调用均值（`summary`），此时 p95 恒为
 *    null：均值不是分位数，不能拿 p50 或均值冒充 P95；
 * 3. 两者都没有时来源为 `none`，所有读数保持 null，由组件渲染 `—`。
 *
 * `hasData` 只由 p50 决定；`lowSample` 只对「有读数且样本不足」的模型成立，
 * 避免把「没数据」也标成低样本。
 */

import type { PublicModelCard } from '@/features/public/pricing/api'

import type { MetricsItem } from './api'
import { kindOf } from './taxonomy'

export type CallKind =
  | 'chat'
  | 'embedding'
  | 'image'
  | 'speech'
  | 'transcription'
  | 'rerank'

export type PerfMeasure = 'ttft' | 'latency'
export type PerfSource = 'test' | 'usage' | 'none'

export interface ModelPerformance {
  source: PerfSource
  measure: PerfMeasure
  p50: number | null
  p95: number | null
  tps: number | null
  successRate: number | null
  samples: number
  lowSample: boolean
  testedAt: string | null
  hasData: boolean
}

export interface ModelEntry {
  card: PublicModelCard
  kind: CallKind
  perf: ModelPerformance
}

/** 低于该样本量的读数标记「低样本」，标注后仍展示。 */
export const LOW_SAMPLE_THRESHOLD = 20

/** chat 的首字延迟是主指标；其它调用类型只有端到端延迟。 */
function defaultMeasure(kind: CallKind): PerfMeasure {
  return kind === 'chat' ? 'ttft' : 'latency'
}

function complete(
  partial: Omit<ModelPerformance, 'hasData' | 'lowSample'>,
): ModelPerformance {
  const hasData = partial.p50 !== null
  return {
    ...partial,
    hasData,
    lowSample: hasData && partial.samples < LOW_SAMPLE_THRESHOLD,
  }
}

function emptyPerformance(measure: PerfMeasure, samples = 0): ModelPerformance {
  return complete({
    source: 'none',
    measure,
    p50: null,
    p95: null,
    tps: null,
    successRate: null,
    samples,
    testedAt: null,
  })
}

/**
 * 按冻结的四步选源规则推导一个模型的读数。
 *
 * 注意「测速行存在但没有可用 P50」不等于命中测速：例如非 chat 模型只有
 * TTFT 百分位，此时继续回退到真实调用均值，而不是伪造一次测速命中。
 */
export function performanceOf(
  kind: CallKind,
  item: MetricsItem | undefined,
): ModelPerformance {
  const fallbackMeasure = defaultMeasure(kind)
  if (item === undefined) return emptyPerformance(fallbackMeasure)

  const test = item.latest_test ?? null
  if (test !== null) {
    const useTtft = kind === 'chat' && test.ttft_ms_p50 !== null
    const useLatency = test.latency_ms_p50 !== null
    if (useTtft || useLatency) {
      return complete({
        source: 'test',
        measure: useTtft ? 'ttft' : 'latency',
        p50: useTtft ? test.ttft_ms_p50 : test.latency_ms_p50,
        // p95 保持服务端原值：缺失就是 null，绝不用 p50 填充。
        p95: useTtft ? test.ttft_ms_p95 : test.latency_ms_p95,
        tps: test.tps ?? null,
        successRate: test.success_rate ?? null,
        samples: test.sample_size ?? 0,
        testedAt: test.tested_at ?? null,
      })
    }
  }

  const summary = item.summary
  const samples = summary.sample_size ?? 0
  if (samples > 0) {
    const useTtft = kind === 'chat' && summary.avg_ttft_ms != null
    const useLatency = summary.avg_latency_ms != null
    if (useTtft || useLatency) {
      return complete({
        source: 'usage',
        measure: useTtft ? 'ttft' : 'latency',
        p50: useTtft ? summary.avg_ttft_ms ?? null : summary.avg_latency_ms ?? null,
        // 均值不是分位数：p95 必须是 null，不能由均值推导。
        p95: null,
        tps: summary.tps ?? null,
        successRate: summary.success_rate ?? null,
        samples,
        testedAt: null,
      })
    }
  }

  return emptyPerformance(fallbackMeasure, samples)
}

/** 目录卡片与指标索引的连接；缺失指标不会阻止卡片进入列表。 */
export function entriesOf(
  cards: readonly PublicModelCard[],
  index: ReadonlyMap<string, MetricsItem>,
): ModelEntry[] {
  return cards.map((card) => {
    const kind = kindOf(card)
    return { card, kind, perf: performanceOf(kind, index.get(card.model_id)) }
  })
}
