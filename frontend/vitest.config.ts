import { fileURLToPath } from 'node:url'

import { defineConfig } from 'vitest/config'

export default defineConfig({
  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./src', import.meta.url)),
    },
    // The shared shell package lives outside this app and carries no
    // node_modules of its own; both React copies must resolve to this one.
    dedupe: ['react', 'react-dom'],
  },
  test: {
    environment: 'jsdom',
    // UI-5 的样式对比度检查只读 tokens.css / tailwind.css，不需要 DOM。
    // jsdom 环境会把全局 URL 换成 whatwg-url 实现，node:url 的
    // fileURLToPath 不识别它（ERR_INVALID_URL_SCHEME），因此该目录改用
    // node 环境；测试文件本身保持冻结、不做修改。
    environmentMatchGlobs: [['src/styles/**', 'node']],
    include: ['src/**/*.{test,spec}.{ts,tsx}'],
    restoreMocks: true,
    setupFiles: ['./test/setup.ts'],
  },
})
