/**
 * 模型库的分类法：能力槽、供应商/上下文/端点 facet、主调用类型与字母标。
 *
 * 能力是自由文本（后端允许新枚举先于前端上线），因此这里的所有计数都以
 * 小写 `value` 归并、以首次出现的原值作为 `label`；未知能力保留原样，
 * 绝不因为不在已知枚举里就丢弃。
 */

import type { PublicModelCard } from '@/features/public/pricing/api'
import { ENDPOINT_TYPES } from '@/features/public/pricing/params'

import type { CallKind } from './performance'

export interface CapabilitySlot {
  /** 小写，用于 URL 与比较。 */
  value: string
  /** 首次出现的原值，用于展示。 */
  label: string
  count: number
}

export interface ModelFacets {
  capabilities: CapabilitySlot[]
  vendors: { value: string; count: number }[]
  contexts: { value: string; count: number }[]
  endpoints: { value: string; count: number }[]
}

const ENDPOINT_TYPE_SET: ReadonlySet<string> = new Set(ENDPOINT_TYPES)

/**
 * 主调用类型的端点优先级：越专门越靠前，chat 只作为兜底。
 * 同一模型同时声明多个端点时，先命中者决定性能指标口径与调用示例路径。
 */
const KIND_BY_ENDPOINT_PRIORITY: ReadonlyArray<readonly [string, CallKind]> = [
  ['rerank', 'rerank'],
  ['images.generations', 'image'],
  ['audio.speech', 'speech'],
  ['audio.transcriptions', 'transcription'],
  ['embeddings', 'embedding'],
  ['chat.completions', 'chat'],
  ['messages', 'chat'],
  ['generatecontent', 'chat'],
]

function compareValues(a: string, b: string): number {
  if (a < b) return -1
  if (a > b) return 1
  return 0
}

/** 上下文分档与后端 `publicContextBucket` 同值；无档案为 null。 */
export function contextBucketOf(length: number | null): string | null {
  if (length === null || !Number.isFinite(length)) return null
  if (length <= 8_192) return '8k'
  if (length <= 32_768) return '32k'
  if (length <= 131_072) return '128k'
  return '200k+'
}

/** 端点 → 主调用类型；未知端点与空端点回退 chat。 */
export function kindForEndpoints(endpoints: readonly string[]): CallKind {
  const present = new Set<string>()
  for (const endpoint of endpoints) {
    if (typeof endpoint !== 'string') continue
    const value = endpoint.trim().toLowerCase()
    if (value !== '') present.add(value)
  }
  for (const [endpoint, kind] of KIND_BY_ENDPOINT_PRIORITY) {
    if (present.has(endpoint)) return kind
  }
  return 'chat'
}

export function kindOf(card: PublicModelCard): CallKind {
  return kindForEndpoints(card.endpoints)
}

/**
 * 大小写变体与重复项只保留首次出现；未知能力原样保留。
 * 条码、条码提示与详情能力列表共用，避免同一模型重复渲染或产生重复 key。
 */
export function uniqueCapabilityNames(capabilities: readonly string[]): string[] {
  const seen = new Set<string>()
  const out: string[] = []
  for (const raw of capabilities) {
    if (typeof raw !== 'string') continue
    const label = raw.trim()
    if (label === '') continue
    const value = label.toLowerCase()
    if (seen.has(value)) continue
    seen.add(value)
    out.push(label)
  }
  return out
}

interface CapabilityTally {
  label: string
  count: number
}

function tallyCapabilities(cards: readonly PublicModelCard[]): CapabilitySlot[] {
  const tallies = new Map<string, CapabilityTally>()
  for (const card of cards) {
    const seen = new Set<string>()
    for (const raw of card.capabilities ?? []) {
      if (typeof raw !== 'string') continue
      const label = raw.trim()
      if (label === '') continue
      const value = label.toLowerCase()
      // 同一模型的重复/大小写变体只计一次。
      if (seen.has(value)) continue
      seen.add(value)
      const tally = tallies.get(value)
      if (tally === undefined) {
        tallies.set(value, { label, count: 1 })
      } else {
        tally.count += 1
      }
    }
  }
  return [...tallies.entries()]
    .map(([value, tally]) => ({ value, label: tally.label, count: tally.count }))
    .sort((a, b) => b.count - a.count || compareValues(a.value, b.value))
}

/**
 * 能力条码的前 `limit` 个槽位。`capabilitySlots(cards, 12)` 是条码；
 * facet 面板需要全集时用 `facetsOf(cards).capabilities`。
 */
export function capabilitySlots(
  cards: readonly PublicModelCard[],
  limit = 12,
): CapabilitySlot[] {
  const slots = tallyCapabilities(cards)
  if (!Number.isFinite(limit) || limit <= 0) {
    return Number.isFinite(limit) ? [] : slots
  }
  return slots.slice(0, Math.floor(limit))
}

function tallyValues(
  cards: readonly PublicModelCard[],
  collect: (card: PublicModelCard) => readonly string[],
): { value: string; count: number }[] {
  const counts = new Map<string, number>()
  for (const card of cards) {
    for (const value of new Set(collect(card))) {
      counts.set(value, (counts.get(value) ?? 0) + 1)
    }
  }
  return [...counts.entries()]
    .map(([value, count]) => ({ value, count }))
    .sort((a, b) => b.count - a.count || compareValues(a.value, b.value))
}

function validEndpoints(card: PublicModelCard): string[] {
  const out: string[] = []
  for (const raw of card.endpoints ?? []) {
    if (typeof raw !== 'string') continue
    const value = raw.trim().toLowerCase()
    if (value !== '' && ENDPOINT_TYPE_SET.has(value)) out.push(value)
  }
  return out
}

/**
 * 基于完整目录的 facet。计数不随当前筛选变化；`value` 一律小写以与 URL
 * 往返一致（vendor 与后端 facet 相同），空 vendor 不产生可渲染条目。
 */
export function facetsOf(cards: readonly PublicModelCard[]): ModelFacets {
  return {
    capabilities: tallyCapabilities(cards),
    vendors: tallyValues(cards, (card) => {
      const vendor = card.vendor.trim()
      return vendor === '' ? [] : [vendor.toLowerCase()]
    }),
    contexts: tallyValues(cards, (card) => {
      const bucket = contextBucketOf(card.context_length)
      return bucket === null ? [] : [bucket]
    }),
    endpoints: tallyValues(cards, validEndpoints),
  }
}

/** CJK 统一表意文字、扩展 A、兼容区、假名与谚文：只取首字。 */
const CJK_PATTERN = /^[぀-ヿ㐀-䶿一-鿿豈-﫿가-힯]/

/**
 * 模型标识方块：优先供应商，缺失时回退展示名。
 * 拉丁文本取前两个字符并大写；CJK 取首字（两个汉字会挤满方块）。
 */
export function monogram(vendor: string, displayName: string): string {
  const source = vendor.trim() || displayName.trim()
  if (source === '') return ''
  const chars = Array.from(source)
  const first = chars[0]
  if (first === undefined) return ''
  if (CJK_PATTERN.test(first)) return first
  return chars.slice(0, 2).join('').toUpperCase()
}
