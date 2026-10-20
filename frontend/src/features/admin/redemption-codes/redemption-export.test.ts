import { describe, expect, it, vi } from 'vitest'

import type { components } from '@/api/schema'

import {
  buildRedemptionExportFile,
  downloadRedemptionCodes,
  redemptionExportFilename,
  REDEMPTION_EXPORT_LIMIT,
} from './redemption-export'

type CreateResponse = components['schemas']['RedemptionCodeCreateResponse']

const NOW = new Date(2026, 9, 20, 12, 0, 0)

function result(overrides: Partial<CreateResponse> = {}): CreateResponse {
  return {
    batch_name: 'launch',
    credits: 100,
    count: 2,
    expires_at: null,
    codes: ['ABCDEFGH12345678ABCDEFGH12', 'ABCDEFGH12345678ABCDEFGH34'],
    ...overrides,
  }
}

function readBlob(blob: Blob): Promise<string> {
  // jsdom 的 Blob 没有 `text()`；FileReader 是它在 jsdom 里可用的读取路径。
  return new Promise((resolve, reject) => {
    const reader = new FileReader()
    reader.onload = () => resolve(String(reader.result))
    reader.onerror = () => reject(reader.error)
    reader.readAsText(blob)
  })
}

describe('redemption export (F-04-17)', () => {
  it('builds a TXT file with one "code - batch - credits" line per code', () => {
    const file = buildRedemptionExportFile(result(), 'txt', {}, NOW)

    expect(file.filename).toBe('redemption-codes-launch-20261020.txt')
    expect(file.mime).toBe('text/plain;charset=utf-8')
    expect(file.content).toBe(
      'ABCDEFGH12345678ABCDEFGH12 - launch - 100\n' +
        'ABCDEFGH12345678ABCDEFGH34 - launch - 100\n',
    )
  })

  it('builds a Markdown table with the frozen header', () => {
    const file = buildRedemptionExportFile(result(), 'markdown', {}, NOW)

    expect(file.filename).toBe('redemption-codes-launch-20261020.md')
    expect(file.mime).toBe('text/markdown;charset=utf-8')
    expect(file.content).toBe(
      [
        '| Code | Batch | Credits |',
        '| --- | --- | ---: |',
        '| ABCDEFGH12345678ABCDEFGH12 | launch | 100 |',
        '| ABCDEFGH12345678ABCDEFGH34 | launch | 100 |',
        '',
      ].join('\n'),
    )
  })

  it('honours the include switches (TC-F-04-17-01)', () => {
    const withoutCredits = buildRedemptionExportFile(
      result(),
      'txt',
      { includeCredits: false },
      NOW,
    )
    expect(withoutCredits.content.split('\n')[0]).toBe(
      'ABCDEFGH12345678ABCDEFGH12 - launch',
    )

    const bare = buildRedemptionExportFile(
      result(),
      'markdown',
      { includeBatch: false, includeCredits: false },
      NOW,
    )
    expect(bare.content).toBe(
      [
        '| Code |',
        '| --- |',
        '| ABCDEFGH12345678ABCDEFGH12 |',
        '| ABCDEFGH12345678ABCDEFGH34 |',
        '',
      ].join('\n'),
    )
  })

  it('sanitizes table cells and the filename without losing the raw batch name', () => {
    const file = buildRedemptionExportFile(
      result({ batch_name: 'Q4 | promo' }),
      'markdown',
      {},
      NOW,
    )
    expect(file.content).toContain('| ABCDEFGH12345678ABCDEFGH12 | Q4 \\| promo | 100 |')

    // `/` 与空白在文件名里非法/易被截断，正文保持原样。
    expect(redemptionExportFilename('A/B 2026', 'txt', NOW)).toBe(
      'redemption-codes-A-B-2026-20261020.txt',
    )
    expect(redemptionExportFilename(null, 'txt', NOW)).toBe(
      'redemption-codes-batch-20261020.txt',
    )
  })

  it('passes the generated Blob to the injected save function', async () => {
    const save = vi.fn()
    const filename = downloadRedemptionCodes(result(), 'txt', {}, save, NOW)

    expect(filename).toBe('redemption-codes-launch-20261020.txt')
    expect(save).toHaveBeenCalledTimes(1)
    const [blob, savedName] = save.mock.calls[0] as [Blob, string]
    expect(savedName).toBe(filename)
    expect(blob.type).toBe('text/plain;charset=utf-8')
    expect(await readBlob(blob)).toContain('ABCDEFGH12345678ABCDEFGH12 - launch - 100')
  })

  it('freezes the export limit at 5000 rows', () => {
    expect(REDEMPTION_EXPORT_LIMIT).toBe(5000)
  })
})
