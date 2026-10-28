import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'

import { describe, expect, it } from 'vitest'

/**
 * 四个公开页（模型库 / 定价 / 排行 / 关于）的页头节奏。
 *
 * 设计约束：页头最后一行文字 → 首个内容容器顶边的距离必须一致，且只能有
 * 一个来源（`--public-head-gap`）。间距是布局属性，jsdom 算不出来，所以这里
 * 锁住「可以被字符串证明」的部分：token 在哪、值是多少、谁在用、谁没有偷偷
 * 再叠加一段自己的 margin。
 */

const srcDir = fileURLToPath(new URL('..', import.meta.url))
const homeCss = readFileSync(`${srcDir}styles/home.css`, 'utf8')

/** 取出某个类名（`.public-shell .name`）的规则块正文。 */
function ruleBody(selector: string): string {
  const escaped = selector.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
  const match = new RegExp(`${escaped}\\s*\\{([^}]*)\\}`).exec(homeCss)
  if (match?.[1] === undefined) throw new Error(`missing rule: ${selector}`)
  return match[1]
}

const PAIRED_WITH_RULE = 'calc(var(--public-head-gap) - 1px)'

describe('public page head → content gap', () => {
  it('is one token, declared on .public-shell', () => {
    // 值本身来自间距刻度（tailwind.css 的 --space-page-gap），这里只锁住
    // 「只有一个来源」这条关系；具体数值由 spacing-standard.test.ts 校验。
    expect(ruleBody('.public-shell')).toContain(
      '--public-head-gap: var(--space-page-gap)',
    )
  })

  it('gives the two rule-less pages one class and one margin', () => {
    expect(ruleBody('.public-shell .public-page-body')).toContain(
      'margin-top: var(--public-head-gap)',
    )
  })

  it('splits the same distance around a hairline on the ruled pages', () => {
    // 排行：39px + 1px 线 = 40px，面板自己不再加 margin。
    expect(ruleBody('.public-shell .rank-head')).toContain(
      `padding-block: 32px ${PAIRED_WITH_RULE}`,
    )
    expect(ruleBody('.public-shell .rank-panel-body')).not.toContain('margin-top')

    // 关于：同样的 39px + 1px 线，事实条顶边正好落在 40px。
    expect(ruleBody('.public-shell .about-hero')).toContain(
      `padding-block: 56px ${PAIRED_WITH_RULE}`,
    )
  })

  it('wires the rule-less pages to that class in their components', () => {
    for (const path of [
      'features/public/models/hero.tsx',
      'features/public/pricing/pricing-page.tsx',
    ]) {
      expect(readFileSync(`${srcDir}${path}`, 'utf8'), path).toContain(
        'public-page-body',
      )
    }
  })

  it('keeps the marketing pricing page (Next app) on the same 40px', () => {
    // `/pricing` 由营销站（website/）承接，不经过 SPA，所以第 4 个公开页
    // 在这里单独锁一次；改了两边任何一边都要一起改。
    const workbench = readFileSync(
      fileURLToPath(
        new URL(
          '../../../website/src/components/landing/pricing-workbench.tsx',
          import.meta.url,
        ),
      ),
      'utf8',
    )
    expect(workbench).toContain('<section className="pb-[40px] pt-14">')
  })
})
