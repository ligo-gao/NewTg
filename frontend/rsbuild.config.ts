import path from 'node:path'

import { defineConfig } from '@rsbuild/core'
import { pluginReact } from '@rsbuild/plugin-react'
import tailwindcss from '@tailwindcss/postcss'
import { TanStackRouterRspack } from '@tanstack/router-plugin/rspack'

const devProxyTarget =
  process.env.PUBLIC_DEV_PROXY_TARGET ?? 'http://localhost:3000'
const basePath = process.env.PUBLIC_BASE_PATH ?? '/'

export default defineConfig({
  plugins: [pluginReact()],
  source: {
    entry: {
      index: './src/main.tsx',
    },
    // The shared public shell ships TypeScript source (one copy for both the
    // SPA and the marketing site), so it must go through the same JSX loader
    // as `src/` instead of being treated as pre-built vendor code.
    include: [path.resolve(process.cwd(), '../packages/public-shell')],
  },
  resolve: {
    alias: {
      '@': path.resolve(process.cwd(), 'src'),
    },
    // `packages/public-shell` lives outside this app, so its `react` import
    // must resolve to this app's copy (one React instance, one hook runtime).
    dedupe: ['react', 'react-dom'],
  },
  html: {
    template: './index.html',
  },
  output: {
    assetPrefix: basePath,
    distPath: {
      root: 'dist',
    },
    sourceMap: false,
  },
  server: {
    port: 5173,
    proxy: {
      '/api': {
        target: devProxyTarget,
        changeOrigin: true,
      },
      '/v1': {
        target: devProxyTarget,
        changeOrigin: true,
        ws: true,
      },
    },
  },
  tools: {
    postcss: {
      postcssOptions: {
        plugins: [tailwindcss()],
      },
    },
    rspack: {
      plugins: [
        TanStackRouterRspack({
          target: 'react',
          autoCodeSplitting: true,
          routesDirectory: './src/routes',
          generatedRouteTree: './src/routeTree.gen.ts',
          // 测试文件与路由同目录但永远不是路由：不忽略时插件会把
          // `_console.test.tsx` 当路由扫描并在每次构建时告警。这里只排除
          // `.test.tsx`，Vitest 的 include 仍会正常收集并运行它。
          routeFileIgnorePattern: '\\.test\\.tsx$',
        }),
      ],
    },
  },
})
