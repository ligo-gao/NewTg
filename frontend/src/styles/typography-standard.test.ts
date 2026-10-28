import { readFileSync, readdirSync } from 'node:fs'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'

import { describe, expect, it } from 'vitest'

/**
 * 排版标准的机器校验（docs/typography-00-排版标准.md）。
 *
 * 这里只断言「可以被字符串证明」的规则：字阶数值、字重集合、字号下限，
 * 以及公开页组件是否还在引用旧字阶。行宽、视觉层级仍需人工按清单核对。
 */

const srcDir = fileURLToPath(new URL('..', import.meta.url))
const stylesDir = `${srcDir}styles/`
const tailwindCss = readFileSync(`${stylesDir}tailwind.css`, 'utf8')
const homeCss = readFileSync(`${stylesDir}home.css`, 'utf8')
const shellCss = readFileSync(
  fileURLToPath(new URL('../../../packages/public-shell/src/shell.css', import.meta.url)),
  'utf8',
)

/** 营销站（website/）——同一套公开页字阶，独立 Tailwind 入口。 */
const websiteSrc = fileURLToPath(new URL('../../../website/src/', import.meta.url))
const websiteCss = readFileSync(`${websiteSrc}app/globals.css`, 'utf8')

/** Tailwind 主题变量的原始声明（`@theme inline` 块）。 */
function themeValue(name: string, css = tailwindCss): string {
  const match = new RegExp(`--${name}:\\s*([^;]+);`).exec(css)
  if (match?.[1] === undefined) throw new Error(`missing theme token --${name}`)
  return match[1].trim()
}

function px(value: string): number {
  const match = /^(\d+(?:\.\d+)?)px$/.exec(value)
  if (match?.[1] === undefined) throw new Error(`not a px value: ${value}`)
  return Number(match[1])
}

/** 目录下所有页面源码（组件 + 路由），用于扫描旧字阶引用。 */
function sourcesUnder(root: string): { path: string; text: string }[] {
  const out: { path: string; text: string }[] = []
  const walk = (dir: string) => {
    for (const entry of readdirSync(dir, { withFileTypes: true })) {
      const path = join(dir, entry.name)
      if (entry.isDirectory()) {
        walk(path)
      } else if (/\.(tsx|ts)$/.test(entry.name) && !/\.test\.|\.spec\./.test(entry.name)) {
        out.push({ path: path.slice(root.length), text: readFileSync(path, 'utf8') })
      }
    }
  }
  walk(root)
  return out
}

const sources = [
  ...sourcesUnder(`${srcDir}features/public/`),
  { path: 'routes/docs.tsx', text: readFileSync(`${srcDir}routes/docs.tsx`, 'utf8') },
]

const websiteSources = sourcesUnder(websiteSrc)

describe('公开页字阶（typography standard）', () => {
  it('以 16px 为基准，按 1.25 倍逐级', () => {
    expect(px(themeValue('text-public-body'))).toBe(16)

    const ladder = [16, 20, 25, 31, 39, 49]
    for (let i = 1; i < ladder.length; i += 1) {
      const ratio = ladder[i]! / ladder[i - 1]!
      expect(ratio, `${ladder[i - 1]} → ${ladder[i]}`).toBeGreaterThanOrEqual(1.23)
      expect(ratio, `${ladder[i - 1]} → ${ladder[i]}`).toBeLessThanOrEqual(1.27)
    }
  })

  it('每一档都落定字号 / 行高 / 字重', () => {
    const levels = [
      'display',
      'hero',
      'title',
      'section',
      'subhead',
      'figure',
      'lead',
      'body-strong',
      'body',
      'label',
      'caption',
    ]
    for (const level of levels) {
      expect(themeValue(`text-public-${level}`), level).not.toBe('')
      expect(themeValue(`text-public-${level}--line-height`), level).not.toBe('')
      expect(themeValue(`text-public-${level}--font-weight`), level).not.toBe('')
    }
  })

  it('行高：正文 1.75、标签 1.57、标题 1.18–1.28', () => {
    expect(themeValue('text-public-body--line-height')).toBe('28px')
    expect(themeValue('text-public-caption--line-height')).toBe('22px')
    expect(themeValue('text-public-label--line-height')).toBe('22px')
    expect(themeValue('text-public-section--line-height')).toBe('34px')
    expect(themeValue('text-public-subhead--line-height')).toBe('28px')
  })

  it('全站只用 400 / 500 / 600 三个字重', () => {
    const weights = new Set(
      ['display', 'hero', 'title', 'section', 'subhead', 'figure', 'lead', 'body-strong', 'body', 'label', 'caption'].map(
        (level) => themeValue(`text-public-${level}--font-weight`),
      ),
    )
    expect([...weights].sort()).toEqual(['400', '500', '600'])
  })

  it('CSS 里不出现 700 及以上字重', () => {
    for (const css of [homeCss, shellCss]) {
      expect(css).not.toMatch(/font-weight:\s*(bold|[7-9]00)/)
    }
  })
})

describe('公开页字号下限', () => {
  /** 装饰性首字母头像：aria-hidden，唯一的例外。 */
  const exemption = 11

  it.each([
    ['home.css', homeCss],
    ['shell.css', shellCss],
  ])('%s 中没有小于 14px 的可见文字', (_name, css) => {
    const sizes = [...css.matchAll(/font-size:\s*(\d+(?:\.\d+)?)px/g)].map((match) =>
      Number(match[1]),
    )
    expect(sizes.length).toBeGreaterThan(0)
    expect(sizes.filter((size) => size < 14).every((size) => size === exemption)).toBe(true)
  })
})

describe('公开页组件只引用新字阶', () => {
  const legacy = /(?<![\w-])text-(meta|badge|card-title|section|page-title|stat|lead)(?![\w-])/

  it('不再使用控制台旧字阶', () => {
    const offenders = sources.filter(({ text }) => legacy.test(text)).map(({ path }) => path)
    expect(offenders).toEqual([])
  })

  it('不写任意像素字号', () => {
    const offenders = sources
      .filter(({ text }) => /text-\[\d+(?:\.\d+)?(?:px|rem)\]/.test(text))
      .map(({ path }) => path)
    expect(offenders).toEqual([])
  })

  it('字体栈：西文 Inter 优先，中文回落到苹方 / 雅黑', () => {
    const stack = /--font-public:\s*([^;]+);/.exec(homeCss)?.[1] ?? ''
    expect(stack).toContain("'Inter'")
    expect(stack).toContain("'PingFang SC'")
    expect(stack).toContain("'Microsoft YaHei'")
  })
})

describe('营销站（website/）与公开页字阶同源', () => {
  const levels = [
    'display',
    'hero',
    'title',
    'section',
    'subhead',
    'figure',
    'lead',
    'body-strong',
    'body',
    'label',
    'caption',
  ]

  it('每一档的取值与 frontend 完全一致', () => {
    for (const level of levels) {
      for (const suffix of ['', '--line-height', '--font-weight']) {
        const name = `text-public-${level}${suffix}`
        expect(themeValue(name, websiteCss), name).toBe(themeValue(name))
      }
    }
  })

  it('基准 16px、行高 28px', () => {
    expect(px(themeValue('text-public-body', websiteCss))).toBe(16)
    expect(themeValue('text-public-body--line-height', websiteCss)).toBe('28px')
    expect(websiteCss).toMatch(/font-size:\s*16px;\s*\n\s*line-height:\s*28px;/)
  })

  it('只用 400 / 500 / 600 三个字重', () => {
    expect(websiteCss).not.toMatch(/font-weight:\s*(bold|[7-9]00)/)
    const bold = websiteSources.filter(({ text }) => /\bfont-bold\b/.test(text))
    expect(bold.map(({ path }) => path)).toEqual([])
  })

  it('不写任意字号与默认字阶，旧 heading 类已换成 token', () => {
    const offenders = websiteSources
      .filter(({ text }) =>
        /text-\[\d+(?:\.\d+)?(?:px|rem)\]/.test(text) ||
        /text-\[clamp\(/.test(text) ||
        /\btext-(?:xs|sm|base|lg|xl|[2-9]xl)\b/.test(text),
      )
      .map(({ path }) => path)
    expect(offenders).toEqual([])
  })

  it('CSS 中可见文字 ≥ 14px', () => {
    const sizes = [...websiteCss.matchAll(/font-size:\s*(\d+(?:\.\d+)?)px/g)].map((match) =>
      Number(match[1]),
    )
    expect(sizes.length).toBeGreaterThan(0)
    expect(sizes.filter((size) => size < 14)).toEqual([])
  })

  it('字体栈：Inter 优先，中文有苹方 / 雅黑回落', () => {
    const stack = /--font-sans:\s*([^;]+);/.exec(websiteCss)?.[1] ?? ''
    expect(stack).toContain('--font-inter')
    expect(stack).toContain('"PingFang SC"')
    expect(stack).toContain('"Microsoft YaHei"')
  })
})
