import { readdirSync, readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'

import { describe, expect, it } from 'vitest'

/**
 * 首屏 CSS 按路由拆分（P2-16）与 auth frame 布局（P2-23）。
 *
 * 这里只能做「字符串可证明」的断言（jsdom 没有布局与级联计算）：
 * 1. 入口只保留全局基础样式，页面级 CSS 由布局/路由自己引入；
 * 2. 公开页布局引 home.css，认证 shell 只引 auth-hall.css，登录页永不下载首页样式；
 * 3. auth frame 自己撑满视口、带页面背景，内容区 flex:1 把页脚推到屏幕底部。
 */

const srcDir = fileURLToPath(new URL('..', import.meta.url))
const read = (path: string): string => readFileSync(`${srcDir}${path}`, 'utf8')

const mainEntry = read('main.tsx')
const publicLayout = read('features/public/public-layout.tsx')
const authHall = read('features/auth/AuthHallShell.tsx')
const legalPage = read('features/auth/LegalPage.tsx')
const authHallCss = read('styles/auth-hall.css')

/** 取出单个选择器的规则块正文。 */
function ruleBody(css: string, selector: string): string {
  const escaped = selector.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
  const match = new RegExp(`${escaped}\\s*\\{([^}]*)\\}`).exec(css)
  if (match?.[1] === undefined) throw new Error(`missing rule: ${selector}`)
  return match[1]
}

function authDirSources(): { name: string; source: string }[] {
  return readdirSync(`${srcDir}features/auth`)
    .filter((name) => name.endsWith('.ts') || name.endsWith('.tsx'))
    .map((name) => ({ name, source: read(`features/auth/${name}`) }))
}

describe('首屏 CSS 按路由拆分（P2-16）', () => {
  it('入口只保留全局基础样式，不再无条件加载页面级 CSS 与字体', () => {
    expect(mainEntry).toContain("import '@/styles/tokens.css'")
    expect(mainEntry).toContain("import '@/styles/tailwind.css'")
    expect(mainEntry).not.toContain("import '@/styles/home.css'")
    expect(mainEntry).not.toContain("import '@/styles/auth-hall.css'")
    expect(mainEntry).not.toContain("import '@/styles/shell-fonts.css'")
  })

  it('公开页布局引入首页样式与自托管字体', () => {
    expect(publicLayout).toContain("import '@/styles/shell-fonts.css'")
    expect(publicLayout).toContain("import '@/styles/home.css'")
    expect(publicLayout).not.toContain('auth-hall.css')
  })

  it('认证 shell 只引入 auth-hall 与字体，登录页不再下载首页样式', () => {
    expect(authHall).toContain("import '@/styles/shell-fonts.css'")
    expect(authHall).toContain("import '@/styles/auth-hall.css'")
    expect(authHall).not.toContain("import '@/styles/home.css'")
  })

  it('法人页沿用认证 frame 的样式与字体', () => {
    expect(legalPage).toContain("import '@/styles/shell-fonts.css'")
    expect(legalPage).toContain("import '@/styles/auth-hall.css'")
    expect(legalPage).not.toContain('home.css')
  })

  it('auth 目录下没有任何文件引入 home.css', () => {
    for (const { name, source } of authDirSources()) {
      expect(source, name).not.toContain('styles/home.css')
    }
  })

  it('公开路由文件不导出页面组件，布局 CSS 才能留在按需 chunk', () => {
    // TanStack Router 只会拆分非导出的 component，且会把路由文件里的
    // 导出声明转换回静态 import；一旦页面组件被 export，它引用的
    // PublicLayout（及其 home.css）就会回到首屏包。
    for (const path of [
      'routes/index.tsx',
      'routes/about.tsx',
      'routes/docs.tsx',
      'routes/legal.$doc.tsx',
      'routes/models/index.tsx',
      'routes/pricing/index.tsx',
      'routes/pricing/$modelId.tsx',
      'routes/rankings/index.tsx',
      'routes/announcements/$id.tsx',
    ]) {
      expect(read(path), path).not.toMatch(
        /export\s+(?:function|const)\s+(?!Route\b)\w*Route\b/,
      )
    }
  })
})

describe('auth frame 的高度与背景（P2-23）', () => {
  it('frame 撑满视口并自带页面背景，页脚下方不再透出白底', () => {
    const body = ruleBody(authHallCss, '.auth-hall-frame')
    expect(body).toContain('min-height: 100vh')
    expect(body).toMatch(/background:\s*var\(--home-paper\)/)
  })

  it('frame 给内容区分配剩余高度，短文档的页脚也贴底', () => {
    expect(authHallCss).toMatch(
      /\.auth-hall-frame > main\s*\{[^}]*flex:\s*1 0 auto/,
    )
  })

  it('LegalPage 去掉内层 min-h-screen，改用 auth frame 分配高度', () => {
    // 只查 className：注释里提到这个类名是说明，不是实际使用。
    expect(legalPage).not.toMatch(/className="[^"]*\bmin-h-screen\b/)
    expect(legalPage).toMatch(/className="[^"]*\bauth-hall-frame\b/)
  })
})
