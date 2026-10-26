import { beforeEach, describe, expect, it, vi } from 'vitest'

/**
 * 渠道路由必须通过 loader 懒加载 admin 域词典：向导第 2 步复用
 * `admin.models.capability_*` / `modality_*` 的标签。这里直接驱动路由
 * 模块的 loader，而不是做源码字符串匹配——删掉 loader 时本用例必须变红。
 */
beforeEach(() => {
  // 词典缓存与激活语言都是模块级状态；让每个用例拿到干净的模块图。
  vi.resetModules()
  globalThis.localStorage?.clear()
})

describe('/admin/channels route dictionary', () => {
  it('loads the admin dictionary so capability labels resolve', async () => {
    const { Route } = await import('./index')
    // TanStack 的 loader 选项既允许函数也允许对象；本路由用函数形式。
    const loader = Route.options.loader as unknown as
      | ((options: unknown) => Promise<unknown>)
      | undefined
    expect(loader).toBeDefined()
    await loader?.({})

    const { getActiveLocale, translate } = await import('@/app/providers/i18n')
    const key = 'admin.models.capability_text'
    expect(translate(getActiveLocale(), key)).not.toBe(key)
  })
})
