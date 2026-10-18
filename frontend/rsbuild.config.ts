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
  },
  resolve: {
    alias: {
      '@': path.resolve(process.cwd(), 'src'),
    },
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
        }),
      ],
    },
  },
})
