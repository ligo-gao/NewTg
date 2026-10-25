import { webcrypto } from 'node:crypto'

import { cleanup } from '@testing-library/react'
import { afterEach, beforeEach } from 'vitest'

/**
 * jsdom ships `Crypto.getRandomValues` but not `SubtleCrypto`. The login
 * encryption path (F-01-25) needs RSA-OAEP and AES-256-GCM, so tests install
 * Node's WebCrypto implementation. Tests never embed real key material: every
 * key pair is generated ephemerally inside the test that needs one.
 */
if (globalThis.crypto?.subtle === undefined) {
  Object.defineProperty(globalThis, 'crypto', {
    configurable: true,
    value: webcrypto,
  })
}

const LANGUAGE_STORAGE_KEY = 'newtg.language'

/**
 * 产品默认语言是 zh-CN；测试断言的是 en 词典的真实文案。写入一次「已选过
 * 语言」的偏好，走的正是真实用户切换过语言的那条路径。
 *
 * 必须在任何模块求值之前执行：`i18n.tsx` 在模块级用 `resolveInitialLocale()`
 * 初始化 `activeLocale`，路由 loader 依赖这份镜像。
 */
globalThis.localStorage?.setItem(LANGUAGE_STORAGE_KEY, 'en')

/**
 * 不少测试文件在自己的 `afterEach` 里清空 localStorage（主题/侧栏等状态），
 * 那会连带抹掉上面这条偏好。每个用例开始前重新写入，测试套件才稳定看到
 * en 文案。在自己的 `beforeEach` 里清空的套件（本钩子跑在它们之前）则由
 * `renderWithProviders` 或本地 render 显式传 `locale="en"` 固定。
 * `i18n.test.ts` 在自己的 `beforeEach` 里清空存储并重新导入模块，因此它
 * 仍然观察真实的产品默认值 zh-CN。
 */
beforeEach(() => {
  globalThis.localStorage?.setItem(LANGUAGE_STORAGE_KEY, 'en')
})

// Vitest runs with `globals: false`, so Testing Library cannot register its
// own auto-cleanup; unmount between tests explicitly to keep DOM state isolated.
afterEach(() => {
  cleanup()
})
