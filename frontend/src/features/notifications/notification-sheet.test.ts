import { describe, expect, it } from 'vitest'

import { notificationHref } from './notification-sheet'

/**
 * `notificationHref` 是通知面板唯一的导航白名单。这里只测纯函数，
 * 不挂载组件：判定规则一旦回归，浏览器就会把不可信 `link_url` 当作
 * 站外跳板，必须在单元层锁死。
 */
describe('notificationHref', () => {
  // 前导 `//` 或 `/\` 会把路径解释成另一台主机；TAB/LF/CR 会被 WHATWG
  // 解析器静默剥离，因此 `"/\t/evil"` 与 `"//evil"` 等价。两种形态都必须
  // 在归一之后被识别并拒绝。
  it.each([
    ['protocol-relative URL', '//evil.example/path'],
    ['backslash protocol-relative URL', '/\\evil.example/path'],
    ['tab-injected protocol-relative URL', '/\t/evil.example'],
    ['newline-injected protocol-relative URL', '/\n/evil.example'],
    ['encoded backslash', '%5Cevil'],
    ['javascript scheme', 'javascript:alert(1)'],
    ['bare relative path', 'evil'],
  ])('rejects %s', (_label, linkUrl) => {
    expect(notificationHref({ link_url: linkUrl })).toBeNull()
  })

  it.each([
    ['single-slash in-site path', '/legacy/x', '/legacy/x'],
    ['absolute http(s) URL', 'https://ok.example', 'https://ok.example'],
  ])('accepts %s', (_label, linkUrl, expected) => {
    expect(notificationHref({ link_url: linkUrl })).toBe(expected)
  })
})
