import { describe, expect, it } from 'vitest'

import { formatYuan } from './money'

describe('formatYuan', () => {
  it('defaults to English grouping and two decimals', () => {
    expect(formatYuan(123456)).toBe('¥1,234.56')
    expect(formatYuan(500000, { decimals: false })).toBe('¥5,000')
  })

  it('honours the active locale for grouping and separators', () => {
    // fr uses a narrow no-break space (U+202F) for thousands and a comma for
    // decimals; the currency symbol stays the module's fixed ¥.
    expect(formatYuan(123456, { locale: 'fr' })).toBe('¥1 234,56')
  })
})
