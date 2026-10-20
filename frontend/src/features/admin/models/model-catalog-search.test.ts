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
})
