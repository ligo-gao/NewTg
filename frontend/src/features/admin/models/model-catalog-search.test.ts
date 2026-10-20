import { describe, expect, it } from 'vitest'

import {
  DEFAULT_MODEL_CATALOG_SEARCH,
  normalizeModelCatalogSearch,
} from './model-catalog-search'

describe('normalizeModelCatalogSearch page sizes', () => {
  it('keeps the page sizes the selector offers', () => {
    const search = normalizeModelCatalogSearch({
      mv_page_size: '50',
      vv_page_size: '10',
    })

    expect(search.mv_page_size).toBe(50)
    expect(search.vv_page_size).toBe(10)
  })

  it.each(['37', '100', '999', '-1', 'abc', ''])(
    'falls back to the default for an unsupported page size (%s)',
    (raw) => {
      const search = normalizeModelCatalogSearch({
        mv_page_size: raw,
        vv_page_size: raw,
      })

      expect(search.mv_page_size).toBe(DEFAULT_MODEL_CATALOG_SEARCH.mv_page_size)
      expect(search.vv_page_size).toBe(DEFAULT_MODEL_CATALOG_SEARCH.vv_page_size)
    },
  )

  // 两个页签的默认档位各自独立：一侧非法只影响自己，不会串到另一侧。
  it('falls back per field instead of borrowing the models default', () => {
    const modelsInvalid = normalizeModelCatalogSearch({
      mv_page_size: '37',
      vv_page_size: '50',
    })
    expect(modelsInvalid.mv_page_size).toBe(DEFAULT_MODEL_CATALOG_SEARCH.mv_page_size)
    expect(modelsInvalid.vv_page_size).toBe(50)

    const vendorsInvalid = normalizeModelCatalogSearch({
      mv_page_size: '10',
      vv_page_size: '37',
    })
    expect(vendorsInvalid.mv_page_size).toBe(10)
    expect(vendorsInvalid.vv_page_size).toBe(DEFAULT_MODEL_CATALOG_SEARCH.vv_page_size)
  })
})
