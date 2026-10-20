import userEvent from '@testing-library/user-event'
import { screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import type { components } from '@/api/schema'
import { renderWithProviders } from '@/test-support/providers'

import { CreateCodesResultDialog } from './create-codes-sheet'

type CreateResponse = components['schemas']['RedemptionCodeCreateResponse']

function result(overrides: Partial<CreateResponse> = {}): CreateResponse {
  return {
    batch_name: 'launch',
    credits: 100,
    count: 2,
    expires_at: null,
    codes: ['ABCDEFGH12345678ABCDEFGH12', 'ABCDEFGH12345678ABCDEFGH34'],
    ...overrides,
  }
}

function readBlob(blob: Blob): Promise<string> {
  // jsdom 的 Blob 没有 `text()`；FileReader 是它在 jsdom 里可用的读取路径。
  return new Promise((resolve, reject) => {
    const reader = new FileReader()
    reader.onload = () => resolve(String(reader.result))
    reader.onerror = () => reject(reader.error)
    reader.readAsText(blob)
  })
}

async function renderResult(
  data: CreateResponse,
  saveFile: (blob: Blob, filename: string) => void,
) {
  return renderWithProviders(
    <CreateCodesResultDialog result={data} onClose={() => {}} saveFile={saveFile} />,
    ['common', 'admin'],
  )
}

describe('CreateCodesResultDialog export (F-04-17)', () => {
  it('downloads the one-time codes as TXT and Markdown locally', async () => {
    const save = vi.fn()
    await renderResult(result(), save)
    const user = userEvent.setup()

    await user.click(screen.getByRole('button', { name: 'Download TXT' }))
    expect(save).toHaveBeenCalledTimes(1)
    const [blob, filename] = save.mock.calls[0] as [Blob, string]
    expect(filename).toMatch(/^redemption-codes-launch-\d{8}\.txt$/)
    expect(await readBlob(blob)).toBe(
      'ABCDEFGH12345678ABCDEFGH12 - launch - 100\n' +
        'ABCDEFGH12345678ABCDEFGH34 - launch - 100\n',
    )

    await user.click(screen.getByRole('button', { name: 'Download Markdown' }))
    expect(save).toHaveBeenCalledTimes(2)
    const [, markdownName] = save.mock.calls[1] as [Blob, string]
    expect(markdownName).toMatch(/^redemption-codes-launch-\d{8}\.md$/)
  })

  it('keeps downloads working when the clipboard is unavailable', async () => {
    // 剪贴板不可用（权限被拒 / 非安全上下文）时，本地下载是唯一兜底。
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: undefined,
    })
    const save = vi.fn()
    await renderResult(result(), save)
    const user = userEvent.setup()

    await user.click(screen.getByRole('button', { name: 'Download TXT' }))

    expect(save).toHaveBeenCalledTimes(1)
  })

  it('disables downloads and explains why above 5000 codes', async () => {
    const codes = Array.from({ length: 5001 }, (_, index) =>
      String(index).padStart(24, '0'),
    )
    await renderResult(result({ count: codes.length, codes }), vi.fn())

    expect(
      (screen.getByRole('button', { name: 'Download TXT' }) as HTMLButtonElement).disabled,
    ).toBe(true)
    expect(
      (screen.getByRole('button', { name: 'Download Markdown' }) as HTMLButtonElement)
        .disabled,
    ).toBe(true)
    expect(
      screen.getByText('Too many codes. Please export in smaller batches.'),
    ).toBeTruthy()
  })
})
