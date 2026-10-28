import { existsSync, readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'

import { describe, expect, it } from 'vitest'

/**
 * 间距刻度与例外登记的机器校验（docs/typography-00-排版标准.md §12–§14）。
 *
 * 这里只断言「可以被字符串证明」的部分：
 * 1. 间距刻度落在 4px 网格上，且 `--space-N` 恒等于 `N × 4px`；
 * 2. 语义间距 token 只能引用刻度，标题上方 ≥ 下方 1.5 倍；
 * 3. 例外表字段齐全、文件存在、复核日期未过期；
 * 4. 存量硬编码只能减少不能增加（棘轮）：数字在代码里，降了就改小。
 */

const styleDir = fileURLToPath(new URL('.', import.meta.url))
const repoRoot = fileURLToPath(new URL('../../../', import.meta.url))

const tokensCss = readFileSync(`${styleDir}tokens.css`, 'utf8')
const exceptions = JSON.parse(
  readFileSync(`${styleDir}typography-exceptions.json`, 'utf8'),
) as {
  exceptions: {
    id: string
    title: string
    scope: string[]
    rule: string
    reason: string
    owner: string
    reviewBy: string
  }[]
}

function scopeFiles(id: string): string[] {
  const entry = exceptions.exceptions.find((item) => item.id === id)
  if (!entry) throw new Error(`missing exception: ${id}`)
  return entry.scope
}

function pxValue(name: string): number {
  const match = new RegExp(`--space-${name}:\\s*(\\d+)px;`).exec(tokensCss)
  if (match?.[1] === undefined) throw new Error(`missing spacing token --space-${name}`)
  return Number(match[1])
}

describe('间距刻度（4px 网格）', () => {
  it('每一档都是 4 的整数倍，且 --space-N 等于 N × 4px', () => {
    const declarations = [...tokensCss.matchAll(/--space-(\d+):\s*(\d+)px;/g)]
    expect(declarations.length).toBeGreaterThanOrEqual(10)
    for (const [, name, value] of declarations) {
      expect(Number(value), `--space-${name}`).toBe(Number(name) * 4)
    }
  })

  it('刻度递增，且不含 0 值（0 由写法本身表达）', () => {
    const names = [...tokensCss.matchAll(/--space-(\d+):\s*(\d+)px;/g)].map(([, name]) =>
      Number(name),
    )
    expect(names).toEqual([...names].sort((a, b) => a - b))
    expect(names).not.toContain(0)
  })

  it('公开页节奏依赖的 40px 在刻度内', () => {
    expect(pxValue('10')).toBe(40)
  })
})

describe('语义间距 token', () => {
  const semantic = [
    'page-gap',
    'block',
    'block-lg',
    'stack',
    'stack-tight',
    'title-above',
    'title-below',
  ]

  it('全部存在，且只能引用刻度', () => {
    for (const name of semantic) {
      const match = new RegExp(`--space-${name}:\\s*var\\(--space-(\\d+)\\);`).exec(tokensCss)
      expect(match, name).not.toBeNull()
    }
  })

  it('标题上方 ≥ 下方 1.5 倍', () => {
    const step = (name: string) => {
      const match = new RegExp(`--space-${name}:\\s*var\\(--space-(\\d+)\\);`).exec(tokensCss)
      return Number(match?.[1])
    }
    expect(step('title-above')).toBeGreaterThanOrEqual(step('title-below') * 1.5)
  })
})

describe('例外登记表', () => {
  it('每条例外字段齐全、编号唯一', () => {
    const ids = new Set<string>()
    for (const item of exceptions.exceptions) {
      expect(item.id, JSON.stringify(item)).toBeTruthy()
      expect(item.title, item.id).toBeTruthy()
      expect(item.rule, item.id).toBeTruthy()
      expect(item.reason, item.id).toBeTruthy()
      expect(item.owner, item.id).toBeTruthy()
      expect(item.scope.length, item.id).toBeGreaterThan(0)
      expect(ids.has(item.id), `duplicate id: ${item.id}`).toBe(false)
      ids.add(item.id)
    }
  })

  it('复核日期是 ISO 日期且未过期', () => {
    const today = new Date().toISOString().slice(0, 10)
    for (const item of exceptions.exceptions) {
      expect(item.reviewBy, item.id).toMatch(/^\d{4}-\d{2}-\d{2}$/)
      expect(item.reviewBy >= today, `${item.id} 已过期，请复核后更新`).toBe(true)
    }
  })

  it('登记的文件与目录都真实存在', () => {
    for (const item of exceptions.exceptions) {
      for (const path of item.scope) {
        expect(existsSync(`${repoRoot}${path}`), `${item.id}: ${path}`).toBe(true)
      }
    }
  })
})

describe('例外之外的硬编码：正字距与全大写', () => {
  const files = [
    'frontend/src/styles/home.css',
    'frontend/src/styles/auth-hall.css',
    'frontend/src/styles/tokens.css',
    'packages/public-shell/src/shell.css',
  ]

  it('正字距只出现在品牌字标（已登记）', () => {
    const allowed = new Set(scopeFiles('brand-wordmark'))
    const offenders = files.filter(
      (path) =>
        /letter-spacing:\s*0?\.0[1-9]\d*em/.test(readFileSync(`${repoRoot}${path}`, 'utf8')) &&
        !allowed.has(path),
    )
    expect(offenders).toEqual([])
  })

  it('text-transform: uppercase 只出现在装饰性头像（已登记）', () => {
    const allowed = new Set(scopeFiles('avatar-monogram'))
    const offenders = files.filter(
      (path) =>
        /text-transform:\s*uppercase/.test(readFileSync(`${repoRoot}${path}`, 'utf8')) &&
        !allowed.has(path),
    )
    expect(offenders).toEqual([])
  })
})

/**
 * 棘轮：现有像素值先冻结，只许往下走。
 * 迁移一处就把它对应的数字改小；加新的字面值会在这里失败。
 */
describe('存量硬编码棘轮', () => {
  type Metric = 'fontSize' | 'fontWeight' | 'spacePx' | 'letterSpacing'
  const baseline: Record<string, Record<Metric, number>> = {
    'frontend/src/styles/home.css': {
      fontSize: 94,
      fontWeight: 50,
      spacePx: 145,
      letterSpacing: 16,
    },
    'frontend/src/styles/auth-hall.css': {
      fontSize: 26,
      fontWeight: 12,
      spacePx: 33,
      letterSpacing: 3,
    },
    'packages/public-shell/src/shell.css': {
      fontSize: 13,
      fontWeight: 7,
      spacePx: 18,
      letterSpacing: 2,
    },
    'website/src/app/globals.css': {
      fontSize: 11,
      fontWeight: 15,
      spacePx: 0,
      letterSpacing: 3,
    },
  }

  const patterns: Record<Metric, RegExp> = {
    fontSize: /font-size:\s*[^;]+;/g,
    fontWeight: /font-weight:\s*[^;]+;/g,
    spacePx: /\b(?:margin|padding)(?:-[a-z]+)?:\s*[^;]*\d+px[^;]*;/g,
    letterSpacing: /letter-spacing:/g,
  }

  it.each(Object.keys(baseline))('%s 的字面值不超过基线', (path) => {
    const text = readFileSync(`${repoRoot}${path}`, 'utf8')
    const limits = baseline[path]!
    for (const metric of Object.keys(patterns) as Metric[]) {
      const count = (text.match(patterns[metric]) ?? []).length
      expect(count, `${path} 的 ${metric}：实际 ${count}，基线 ${limits[metric]}`).toBeLessThanOrEqual(
        limits[metric],
      )
    }
  })
})
