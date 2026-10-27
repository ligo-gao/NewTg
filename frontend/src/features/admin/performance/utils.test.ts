import { describe, expect, it } from 'vitest'

import { ApiError } from '@/api/errors'

import type { PerfConfig, PerformanceLogFile } from './api'
import {
  KNOWN_PROBLEM_CODES,
  PERF_STATUSES,
  diskUsagePercent,
  formatBytes,
  formatClock,
  formatDateTime,
  formatLatencyMs,
  formatMilliseconds,
  formatMs,
  formatPercent,
  formatRatio,
  perfStatusKey,
  perfStatusTone,
  previewLogCleanup,
  problemCode,
  sparkPath,
  usageTone,
  validateCleanupValue,
  vitalTone,
} from './utils'

function file(name: string, size: number, modTime: string): PerformanceLogFile {
  return { name, size, mod_time: modTime }
}

describe('byte and numeric formatting', () => {
  it('keeps one decimal for KB and above and groups bytes below 1 KiB', () => {
    expect(formatBytes(0, 'en')).toBe('0 B')
    expect(formatBytes(512, 'en')).toBe('512 B')
    expect(formatBytes(1536, 'en')).toBe('1.5 KB')
    expect(formatBytes(812 * 1024 * 1024, 'en')).toBe('812.0 MB')
    expect(formatBytes(2 * 1024 ** 3, 'en')).toBe('2.0 GB')
  })

  it('returns the placeholder for unavailable values', () => {
    expect(formatBytes(Number.NaN, 'en')).toBe('—')
    expect(formatBytes(-1, 'en')).toBe('—')
    expect(formatDateTime(null, 'en')).toBe('—')
    expect(formatDateTime('not-a-date', 'en')).toBe('—')
    expect(formatClock(0, 'en')).toBe('—')
    expect(formatMilliseconds(Number.NaN, 'en')).toBe('—')
  })

  it('formats a valid timestamp', () => {
    expect(formatDateTime('2026-09-01T00:00:00Z', 'en')).not.toBe('—')
    expect(formatClock(Date.parse('2026-10-20T12:03:45Z'), 'en')).not.toBe('—')
    expect(formatMilliseconds(12, 'en')).toBe('12 ms')
  })
})

describe('disk usage thresholds', () => {
  it('computes the percentage against max_mb and clamps it', () => {
    expect(diskUsagePercent(512 * 1024 * 1024, 2048)).toBe(25)
    expect(diskUsagePercent(0, 0)).toBe(0)
    expect(diskUsagePercent(3 * 1024 * 1024 * 1024, 1024)).toBe(100)
  })

  it('maps <70 to default, 70–90 to warning and >90 to danger', () => {
    expect(usageTone(0)).toBe('default')
    expect(usageTone(69.9)).toBe('default')
    expect(usageTone(70)).toBe('warning')
    expect(usageTone(90)).toBe('warning')
    expect(usageTone(90.1)).toBe('danger')
  })
})

describe('cleanup value validation', () => {
  it('accepts integers in 1–36500 and separates empty from out-of-range', () => {
    expect(validateCleanupValue('30')).toEqual({ value: 30, error: undefined })
    expect(validateCleanupValue('1')).toEqual({ value: 1, error: undefined })
    expect(validateCleanupValue('36500')).toEqual({
      value: 36500,
      error: undefined,
    })
    expect(validateCleanupValue('')).toEqual({ error: 'required' })
    expect(validateCleanupValue('0')).toEqual({ error: 'range' })
    expect(validateCleanupValue('36501')).toEqual({ error: 'range' })
    expect(validateCleanupValue('1.5')).toEqual({ error: 'range' })
  })
})

describe('previewLogCleanup', () => {
  const now = Date.parse('2026-10-20T12:00:00Z')
  const files = [
    file('newtg-today.log', 100, '2026-10-20T11:00:00Z'),
    file('newtg-mid.log', 200, '2026-10-15T11:00:00Z'),
    file('newtg-old.log', 300, '2026-09-01T11:00:00Z'),
  ]

  it('by_days deletes files older than the cutoff', () => {
    const preview = previewLogCleanup(files, 'by_days', 30, now)
    expect(preview).toEqual({ count: 1, bytes: 300, activeSkipped: false })
  })

  it('by_count keeps the newest N files', () => {
    const preview = previewLogCleanup(files, 'by_count', 2, now)
    expect(preview).toEqual({ count: 1, bytes: 300, activeSkipped: false })

    expect(previewLogCleanup(files, 'by_count', 20, now)).toEqual({
      count: 0,
      bytes: 0,
      activeSkipped: false,
    })
  })

  it('never counts the active file (newest mtime), even when it falls in the window', () => {
    const staleActive = [
      file('newtg-active.log', 400, '2026-04-01T11:00:00Z'),
      file('newtg-older.log', 500, '2026-03-01T11:00:00Z'),
    ]
    const preview = previewLogCleanup(staleActive, 'by_days', 30, now)
    expect(preview).toEqual({ count: 1, bytes: 500, activeSkipped: true })
  })

  it('returns an empty preview for an invalid value', () => {
    expect(previewLogCleanup(files, 'by_days', 0, now)).toEqual({
      count: 0,
      bytes: 0,
      activeSkipped: false,
    })
  })

  it('breaks mtime ties by file name descending, matching the server', () => {
    // 服务端 sortPerfFilesDesc：mtime 相同按文件名倒序；活动文件取排序后首元素。
    const tied = [
      file('newtg-a.log', 100, '2026-10-19T11:00:00Z'),
      file('newtg-b.log', 200, '2026-10-19T11:00:00Z'),
    ]
    // by_count 保留 1 个：保留 b，删除 a；活动文件是 b 而不是先出现的 a。
    expect(previewLogCleanup(tied, 'by_count', 1, now)).toEqual({
      count: 1,
      bytes: 100,
      activeSkipped: false,
    })
    // 同 mtime 且都在删除窗口内时，活动文件（b）被跳过，只统计 a。
    const preview = previewLogCleanup(tied, 'by_days', 1, now)
    expect(preview).toEqual({ count: 1, bytes: 100, activeSkipped: true })
  })
})

describe('problem code extraction', () => {
  it('reads the detail prefix first, then the type suffix', () => {
    const fromDetail = new ApiError({
      status: 422,
      type: 'https://newtg.dev/problems/something_else',
      title: 'Invalid',
      detail: 'INVALID_MODE: mode must be by_count or by_days',
      kind: 'validation',
    })
    expect(problemCode(fromDetail)).toBe('invalid_mode')
    expect(KNOWN_PROBLEM_CODES.has(problemCode(fromDetail))).toBe(true)

    const fromType = new ApiError({
      status: 409,
      type: 'https://newtg.dev/problems/instance_mismatch',
      title: 'Conflict',
      kind: 'conflict',
    })
    expect(problemCode(fromType)).toBe('instance_mismatch')
  })
})

describe('performance readouts', () => {
  it('formats 0–1 ratios as percentage with one decimal', () => {
    expect(formatPercent(0.962)).toBe('96.2%')
    expect(formatPercent(1)).toBe('100.0%')
    expect(formatPercent(null)).toBe('—')
    expect(formatPercent(Number.NaN)).toBe('—')
  })

  it('formats an unlabelled ratio with the requested digits', () => {
    expect(formatRatio(0.213, 2)).toBe('0.21')
    expect(formatRatio(0.213, 0)).toBe('0')
    expect(formatRatio(null, 2)).toBe('—')
  })

  it('formats milliseconds and keeps the no-sample placeholder', () => {
    expect(formatMs(null)).toBe('—')
    expect(formatMs(Number.NaN)).toBe('—')
    expect(formatMs(420)).toBe('420ms')
    expect(formatMs(1_840)).toBe('1.8s')
  })

  it('never formats an over-30s bound as a finite latency', () => {
    // +Inf 桶：值与布尔同时存在时，布尔优先—值只是下界，不是可展示的读数。
    expect(formatLatencyMs(30_000, true)).toBe('>30s')
    expect(formatLatencyMs(null, true)).toBe('>30s')
    expect(formatLatencyMs(2_400, false)).toBe('2.4s')
    expect(formatLatencyMs(null, false)).toBe('—')
  })
})

describe('status mapping', () => {
  it('maps every model status to its badge tone', () => {
    expect(perfStatusTone('incident')).toBe('danger')
    expect(perfStatusTone('degraded')).toBe('warning')
    expect(perfStatusTone('no_sample')).toBe('neutral')
    expect(perfStatusTone('normal')).toBe('success')
  })

  it('falls back to neutral for a status outside the contract', () => {
    expect(perfStatusTone('paused')).toBe('neutral')
    expect(perfStatusTone('')).toBe('neutral')
  })

  it('keeps the four contract statuses and their i18n keys', () => {
    expect(PERF_STATUSES).toEqual([
      'normal',
      'degraded',
      'incident',
      'no_sample',
    ])
    expect(perfStatusKey('no_sample')).toBe(
      'admin.performance.models.status_no_sample',
    )
  })

  it('maps vital statuses, with unknown and unlisted values as neutral', () => {
    expect(vitalTone('ok')).toBe('success')
    expect(vitalTone('warn')).toBe('warning')
    expect(vitalTone('crit')).toBe('danger')
    expect(vitalTone('unknown')).toBe('neutral')
    expect(vitalTone('melted')).toBe('neutral')
  })

  it('keeps schedule.source three-valued through the generated type', () => {
    const sources: NonNullable<PerfConfig['schedule']>['source'][] = [
      'settings',
      'env',
      'mixed',
    ]
    expect(sources).toHaveLength(3)
  })
})

describe('sparkPath', () => {
  it('starts with M and never emits NaN for degenerate inputs', () => {
    expect(sparkPath([], 100, 24)).toBe('')
    expect(sparkPath([3], 100, 24)).toMatch(/^M/)
    expect(sparkPath([3], 100, 24)).not.toContain('NaN')
    expect(sparkPath([null, null], 100, 24)).toBe('')
    expect(sparkPath([1, null, 2], 100, 24)).not.toContain('NaN')
  })

  it('breaks the line at null points instead of connecting across the hole', () => {
    const path = sparkPath([1, null, 2], 100, 24)
    // 两个非 null 段各起一条子路径，中间没有 L 跨过 null 的位置。
    expect(path.match(/M/g)).toHaveLength(2)
    expect(path).not.toContain('L')
  })

  it('draws a horizontal line when every value is equal', () => {
    const path = sparkPath([1, 1, 1], 100, 24)
    const ys = path
      .split(' ')
      .map((command) => Number(command.split(',')[1]))
    expect(ys).toHaveLength(3)
    expect(new Set(ys).size).toBe(1)
    expect(ys.every((y) => Number.isFinite(y))).toBe(true)
  })
})

