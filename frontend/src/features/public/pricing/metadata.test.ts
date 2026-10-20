import { describe, expect, it } from 'vitest'

import {
  KNOWN_CAPABILITIES,
  KNOWN_CONTEXT_BUCKETS,
  KNOWN_ENDPOINTS,
  KNOWN_MODALITIES,
  capabilityLabel,
  contextBucketLabel,
  endpointLabel,
  modalityLabel,
} from './metadata'
import { CONTEXT_BUCKETS, ENDPOINT_TYPES } from './params'

/**
 * metadata.ts 手工镜像了后端的冻结枚举（前端无法 import Go 常量）。这组
 * 测试把镜像值锁在「本地常量 + 后端冻结枚举」上：任一侧新增枚举而忘记
 * 同步前端文案时，未知值虽会原样回退显示，但会在这里先暴露出来。
 */
const BACKEND_MODEL_META_CAPABILITIES = [
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
]

const BACKEND_MODEL_META_MODALITIES = ['text', 'image', 'audio', 'video']

function sorted(values: Iterable<string>): string[] {
  return [...values].sort()
}

describe('metadata enum mirror (D1e)', () => {
  it('mirrors the backend frozen enums and local URL constants', () => {
    // backend/internal/store/model_meta_validate.go
    expect(sorted(KNOWN_CAPABILITIES)).toEqual(sorted(BACKEND_MODEL_META_CAPABILITIES))
    expect(sorted(KNOWN_MODALITIES)).toEqual(sorted(BACKEND_MODEL_META_MODALITIES))
    // backend/internal/httpapi/handlers_public_pricing.go
    expect(sorted(KNOWN_CONTEXT_BUCKETS)).toEqual(sorted(CONTEXT_BUCKETS))
    expect(sorted(KNOWN_ENDPOINTS)).toEqual(sorted(ENDPOINT_TYPES))
  })

  it('labels every mirrored value and falls back to the raw enum for unknown ones', () => {
    const t = (key: string) => `t:${key}`
    for (const value of KNOWN_CAPABILITIES) {
      expect(capabilityLabel(value, t)).not.toBe(value)
    }
    for (const value of KNOWN_MODALITIES) {
      expect(modalityLabel(value, t)).not.toBe(value)
    }
    for (const value of KNOWN_ENDPOINTS) {
      expect(endpointLabel(value, t)).not.toBe(value)
    }
    for (const value of KNOWN_CONTEXT_BUCKETS) {
      expect(contextBucketLabel(value, t)).not.toBe(value)
    }

    // 后端允许新枚举先于前端文案上线：未知值原样展示，不回退成键名。
    expect(capabilityLabel('telepathy', t)).toBe('telepathy')
    expect(modalityLabel('hologram', t)).toBe('hologram')
    expect(endpointLabel('graphql', t)).toBe('graphql')
    expect(contextBucketLabel('1m', t)).toBe('1m')
  })
})
