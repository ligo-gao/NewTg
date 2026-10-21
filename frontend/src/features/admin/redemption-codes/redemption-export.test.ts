import { describe, expect, it, vi } from 'vitest'

import type { components } from '@/api/schema'

import {
  buildRedemptionExportFile,
  buildRedemptionListExportFile,
  downloadRedemptionCodes,
  redemptionExportFilename,
  REDEMPTION_EXPORT_LIMIT,
} from './redemption-export'

type CreateResponse = components['schemas']['RedemptionCodeCreateResponse']
type RedemptionCode = components['schemas']['RedemptionCode']

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

// R-49：列表导出元数据列必须按权限裁剪——没有 users:read 时既不输出
// used_by 列，也不输出任何用户 ID。
describe('redemption metadata export (C3/R-49)', () => {
  const rows = [
    codeRow({ id: 1, batch_name: 'launch', credits: 100, status: 'used', used_by: 7, used_at: '2026-10-02T10:00:00Z' }),
    codeRow({ id: 2, batch_name: 'launch', credits: 50, status: 'unused' }),
  ]

  function codeRow(overrides: Partial<RedemptionCode> = {}): RedemptionCode {
    return {
      id: 12,
      batch_name: 'launch',
      credits: 10000,
      status: 'unused',
      code_mask: '••••••',
      created_by: 1,
      used_by: null,
      used_at: null,
      expires_at: null,
      created_at: '2026-10-01T10:00:00Z',
      updated_at: '2026-10-01T10:00:00Z',
      ...overrides,
    }
  }

  it('never contains code values, only metadata columns', () => {
    const file = buildRedemptionListExportFile(rows, 'markdown', { includeUsedBy: true }, NOW)
    expect(file.content).not.toContain('code_mask')
    expect(file.content).not.toContain('••••••')
    expect(file.content).toContain('| ID | Batch | Credits | Status | Created at | Used at | Used by |')
    expect(file.content).toContain('| 1 | launch | 100 | used | 2026-10-01T10:00:00Z | 2026-10-02T10:00:00Z | 7 |')
    // 未使用的行：时间与使用人为空串，而不是 UI 的「—」。
    expect(file.content).toContain('| 2 | launch | 50 | unused | 2026-10-01T10:00:00Z |  |  |')
  })

  it('omits the used_by column entirely without users:read (fail-closed default)', () => {
    const file = buildRedemptionListExportFile(rows, 'markdown', {}, NOW)
    expect(file.content).not.toMatch(/Used by/)
    expect(file.content).not.toContain('| 7 |')
    // 默认即使调用方没传 includeUsedBy，也不能把 7 这个用户 ID 写进文件。
    for (const line of file.content.split('\n')) {
      expect(line).not.toMatch(/\b7\b/)
    }
  })

  it('writes tab-separated TXT with a header and honours switches', () => {
    const file = buildRedemptionListExportFile(
      rows,
      'txt',
      { includeBatch: false, includeStatus: false, includeUsedBy: false },
      NOW,
    )
    expect(file.filename).toBe('redemption-codes-metadata-20261020.txt')
    const lines = file.content.trimEnd().split('\n')
    expect(lines[0]).toBe('ID\tCredits\tCreated at\tUsed at')
    expect(lines[1]).toBe('1\t100\t2026-10-01T10:00:00Z\t2026-10-02T10:00:00Z')
  })

  it('rejects an oversized selection instead of generating a giant blob', () => {
    expect(() =>
      buildRedemptionListExportFile(
        Array.from({ length: REDEMPTION_EXPORT_LIMIT + 1 }, (_, index) =>
          codeRow({ id: index + 1 }),
        ),
        'txt',
      ),
    ).toThrow(/exceeds limit/)
  })
})
