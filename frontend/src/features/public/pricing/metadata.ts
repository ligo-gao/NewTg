/**
 * 模型档案枚举的可读名（D1e）。
 *
 * 技术枚举（`function_calling`、`chat.completions`）不能直接当英文句子展示：
 * 它们既不是用户语言，也会在 7 语界面里显得格格不入。这里只翻译**已知**
 * 枚举，未知值原样返回——后端允许新枚举先于前端文案上线，此时展示原值
 * 比显示 `public.pricing.capability.xxx` 这种键名更可读（i18n 缺键会回退键名）。
 */

import type { TranslateFn } from '@/shared/lib/i18n-context'

/** 与后端 model_meta_validate.go 的 modelMetaCapabilities 同值。 */
const KNOWN_CAPABILITIES: ReadonlySet<string> = new Set([
  'text',
  'vision',
  'audio',
  'video',
  'function_calling',
  'tool_use',
  'reasoning',
  'embedding',
  'rerank',
  'image_generation',
  'speech',
  'transcription',
])

/** 与后端 modelMetaModalities 同值。 */
const KNOWN_MODALITIES: ReadonlySet<string> = new Set([
  'text',
  'image',
  'audio',
  'video',
])

/**
 * 与后端 publicEndpointMapping 同值，键为 URL 里的小写形态。
 * 注意 `generateContent` 的 value 在 facet 中被后端转成小写，故此处也用小写。
 */
const KNOWN_ENDPOINTS: ReadonlySet<string> = new Set([
  'chat.completions',
  'messages',
  'embeddings',
  'images.generations',
  'audio.speech',
  'audio.transcriptions',
  'rerank',
  'generatecontent',
])

/**
 * 上下文分档只有四个固定桶。保留技术标签（8K/32K/…）但按语言本地化区间
 * 描述：用户需要知道「这个桶覆盖多大」，而不是记住后端枚举拼写。
 */
const KNOWN_CONTEXT_BUCKETS: ReadonlySet<string> = new Set([
  '8k',
  '32k',
  '128k',
  '200k+',
])

export function capabilityLabel(value: string, t: TranslateFn): string {
  const key = value.toLowerCase()
  return KNOWN_CAPABILITIES.has(key)
    ? t(`public.pricing.capability.${key}`)
    : value
}

export function modalityLabel(value: string, t: TranslateFn): string {
  const key = value.toLowerCase()
  return KNOWN_MODALITIES.has(key) ? t(`public.pricing.modality.${key}`) : value
}

export function endpointLabel(value: string, t: TranslateFn): string {
  const key = value.toLowerCase()
  const slug = key.replace(/[^a-z0-9]+/g, '_').replace(/^_|_$/g, '')
  return KNOWN_ENDPOINTS.has(key)
    ? t(`public.pricing.endpoint.${slug}`)
    : value
}

export function contextBucketLabel(value: string, t: TranslateFn): string {
  const key = value.toLowerCase()
  const slug = key.replace(/[^a-z0-9]+/g, '_').replace(/^_|_$/g, '')
  return KNOWN_CONTEXT_BUCKETS.has(key)
    ? t(`public.pricing.context_bucket.${slug}`)
    : value
}
