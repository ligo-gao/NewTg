/**
 * 补全模型档案向导的目录读取（纯 IO，不渲染、不读全局状态）。
 *
 * 模型目录与供应商目录都按契约上限翻页；`MAX_PAGES` 封顶避免上游异常
 * `total` 时无限翻页。查询键留在容器里，本文件只负责请求。
 */
import { apiClient } from '@/api/client'
import { requireData } from '@/api/result'
import type { components } from '@/api/schema'

type ModelMeta = components['schemas']['ModelMeta']

/** `/api/models`、`/api/vendors` 的每页上限（契约上限 100）。 */
export const PAGE_SIZE = 100
/** 目录是百级规模；封顶避免上游异常 total 时无限翻页。 */
export const MAX_PAGES = 10

export interface VendorDirectory {
  /** `lower(trim(name))` → id，与 Task 6 的建商去重口径一致。 */
  ids: Map<string, number>
  /** id → 原始名称，用于把下拉选中的 id 还原成写入体里的名字。 */
  nameById: Map<number, string>
  /** 原始大小写，仅用于下拉展示。 */
  names: string[]
}

export async function loadModelCatalog(): Promise<Map<string, ModelMeta>> {
  const catalog = new Map<string, ModelMeta>()
  let loaded = 0
  for (let page = 1; page <= MAX_PAGES; page += 1) {
    const data = requireData(
      await apiClient.GET('/api/models', {
        params: { query: { p: page, page_size: PAGE_SIZE } },
      }),
    )
    const items = data.items ?? []
    for (const item of items) catalog.set(item.model_name, item)
    loaded += items.length
    if (items.length === 0 || loaded >= data.total) break
  }
  return catalog
}

export async function loadVendorDirectory(): Promise<VendorDirectory> {
  const ids = new Map<string, number>()
  const nameById = new Map<number, string>()
  const names: string[] = []
  let loaded = 0
  for (let page = 1; page <= MAX_PAGES; page += 1) {
    const data = requireData(
      await apiClient.GET('/api/vendors', {
        params: { query: { p: page, page_size: PAGE_SIZE } },
      }),
    )
    const items = data.items ?? []
    for (const vendor of items) {
      const name = vendor.name.trim()
      const key = name.toLowerCase()
      if (key === '') continue
      if (!ids.has(key)) names.push(name)
      ids.set(key, vendor.id)
      nameById.set(vendor.id, name)
    }
    loaded += items.length
    if (items.length === 0 || loaded >= data.total) break
  }
  names.sort((left, right) => left.localeCompare(right))
  return { ids, nameById, names }
}
