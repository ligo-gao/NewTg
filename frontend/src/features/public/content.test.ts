import { describe, expect, it } from 'vitest'

import { TRACE_STAGES } from './content'

describe('public content constants', () => {
  it('documents the six request stages once, in request order', () => {
    expect(TRACE_STAGES).toEqual([
      'auth',
      'quota',
      'route',
      'translate',
      'stream',
      'settle',
    ])
    expect(new Set(TRACE_STAGES).size).toBe(6)
  })
})
