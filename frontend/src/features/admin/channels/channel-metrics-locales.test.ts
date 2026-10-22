import { describe, expect, it } from 'vitest'

import en from '@/locales/en/common.json'
import fr from '@/locales/fr/common.json'
import ja from '@/locales/ja/common.json'
import ru from '@/locales/ru/common.json'
import vi from '@/locales/vi/common.json'
import zhCN from '@/locales/zh-CN/common.json'
import zhTW from '@/locales/zh-TW/common.json'

/**
 * W16-1：指标面板与列表列用到的 7 语文案必须全部存在且非空。
 *
 * `check_locale_parity.sh` 保证「键集合与 en 一致」；本用例是前端测试侧的
 * 双保险，直接把本条任务新增的键钉在测试里，避免有人删键后 parity 只看
 * 「7 语一起缺」而误判为一致。
 */

const LOCALES = {
  en,
  'zh-CN': zhCN,
  'zh-TW': zhTW,
  fr,
  ru,
  ja,
  vi,
} as const

const REQUIRED_KEYS = [
  'channels.table.successRate24h',
  'channels.table.avgLatency24h',
  'channels.metrics.title',
  'channels.metrics.trend',
  'channels.metrics.refresh',
  'channels.metrics.retry',
  'channels.metrics.empty',
  'channels.metrics.error',
  'channels.metrics.offline',
  'channels.metrics.requests',
  'channels.metrics.successRate',
  'channels.metrics.avgLatency',
  'channels.metrics.avgFirstToken',
  'channels.metrics.avgCost',
  'channels.metrics.usedCredits',
  'channels.metrics.range.24h',
  'channels.metrics.range.7d',
] as const

function lookup(dictionary: unknown, path: string): unknown {
  return path.split('.').reduce<unknown>((node, key) => {
    if (typeof node !== 'object' || node === null) return undefined
    return (node as Record<string, unknown>)[key]
  }, dictionary)
}

describe('channel metrics locale keys', () => {
  for (const [locale, dictionary] of Object.entries(LOCALES)) {
    it(`${locale} defines every metrics key as a non-empty string`, () => {
      for (const key of REQUIRED_KEYS) {
        const value = lookup(dictionary, key)
        expect(typeof value, `${locale} ${key}`).toBe('string')
        expect((value as string).trim().length, `${locale} ${key}`).toBeGreaterThan(0)
      }
    })
  }
})
