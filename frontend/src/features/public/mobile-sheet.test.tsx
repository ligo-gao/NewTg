import { fireEvent, waitFor } from '@testing-library/react'
import { useState } from 'react'
import { describe, expect, it } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { MobileSheet } from './mobile-sheet'

function SheetHarness() {
  const [open, setOpen] = useState(false)
  return (
    <div>
      <button type="button" onClick={() => setOpen(true)}>
        Open filters
      </button>
      <MobileSheet
        open={open}
        onOpenChange={setOpen}
        title="Filters"
        description="Choose the facets to apply."
        closeLabel="Close"
      >
        <label>
          Group
          <input aria-label="Group" />
        </label>
      </MobileSheet>
    </div>
  )
}

describe('MobileSheet (F-09-23)', () => {
  it('reuses the shared modal primitive and closes with Escape', async () => {
    const { getByRole, queryByRole, findByRole } = await renderWithProviders(<SheetHarness />, [
      'public',
    ])

    fireEvent.click(getByRole('button', { name: 'Open filters' }))
    const dialog = await findByRole('dialog', { name: 'Filters' })
    expect(dialog.getAttribute('aria-modal')).toBe('true')
    expect(dialog.textContent).toContain('Choose the facets to apply.')

    fireEvent.keyDown(document, { key: 'Escape' })
    await waitFor(() => {
      expect(queryByRole('dialog')).toBeNull()
    })
  })

  it('stacks as a bottom sheet at mobile width and keeps the side sheet on desktop', async () => {
    const { getByRole, findByRole } = await renderWithProviders(<SheetHarness />, ['public'])

    fireEvent.click(getByRole('button', { name: 'Open filters' }))
    const dialog = await findByRole('dialog', { name: 'Filters' })

    // 底部 Sheet 由响应式类实现：窄屏贴底、全宽、顶部圆角；md 以上保持侧栏形态。
    expect(dialog.className).toContain('max-md:mt-auto')
    expect(dialog.className).toContain('max-md:h-auto')
    expect(dialog.className).toContain('max-md:max-w-none')
    expect(dialog.className).toContain('max-md:rounded-t-xl')
    // 拖柄只作装饰，不能进入可访问名称。
    expect(dialog.querySelector('[data-testid="mobile-sheet-handle"]')).toBeTruthy()
    expect(dialog.querySelector('[data-testid="mobile-sheet-handle"]')?.getAttribute('aria-hidden')).toBe(
      'true',
    )
  })

  it('exposes an explicit close control', async () => {
    const { getByRole, findByRole, queryByRole } = await renderWithProviders(<SheetHarness />, [
      'public',
    ])

    fireEvent.click(getByRole('button', { name: 'Open filters' }))
    await findByRole('dialog', { name: 'Filters' })
    fireEvent.click(getByRole('button', { name: 'Close' }))
    await waitFor(() => {
      expect(queryByRole('dialog')).toBeNull()
    })
  })
})
