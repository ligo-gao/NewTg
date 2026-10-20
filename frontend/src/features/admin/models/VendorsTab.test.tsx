import { useState } from 'react'
import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { screen, waitFor, within } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it } from 'vitest'

import type { components } from '@/api/schema'
import { renderWithProviders } from '@/test-support/providers'
import { useToastStore } from '@/shared/ui/toast'

import { ModelCatalogPage } from './ModelCatalogPage'
import {
  DEFAULT_MODEL_CATALOG_SEARCH,
  type ModelCatalogSearch,
} from './model-catalog-search'

type Vendor = components['schemas']['Vendor']

const ORIGIN = 'http://localhost:3000'
const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())
beforeEach(() => {
  useToastStore.setState({ toasts: [] })
  installHandlers()
})

function vendorRow(overrides: Partial<Vendor> = {}): Vendor {
  return {
    id: 1,
    name: 'OpenAI',
    description: 'Frontier models',
    icon: null,
    status: 1,
    created_at: '2026-01-01T00:00:00Z',
    updated_at: '2026-10-01T10:00:00Z',
    model_count: 0,
    version: 'v-openai-0001',
    ...overrides,
  }
}

interface InstallOptions {
  items?: Vendor[]
  total?: number
  applyStatus?: number
  deleteStatus?: number
  deleteReferenceCount?: number
  onApply?: (body: Record<string, unknown>) => void
  onPreview?: (body: Record<string, unknown>) => void
}

function installHandlers({
  items = [vendorRow(), vendorRow({ id: 2, name: 'Anthropic', description: null, model_count: 3 })],
  total = 2,
  applyStatus = 200,
  deleteStatus = 204,
  deleteReferenceCount,
  onApply,
  onPreview,
}: InstallOptions = {}) {
  server.use(
    http.get(`${ORIGIN}/api/me`, () =>
      HttpResponse.json({
        username: 'alice',
        role: 'admin',
        status: 'enabled',
        permissions: [
          'channels:read',
          'channels:manage',
          'channels:manage_sensitive',
          'pricing:manage',
        ],
      }),
    ),
    http.get(`${ORIGIN}/api/vendors`, () =>
      HttpResponse.json({ items, total, page: 1, page_size: 20 }),
    ),
    http.get(`${ORIGIN}/api/vendors/search`, () =>
      HttpResponse.json({
        items: [
          { id: 1, name: 'OpenAI', status: 1 },
          { id: 2, name: 'Anthropic', status: 1 },
          { id: 3, name: 'Google', status: 1 },
        ],
      }),
    ),
    http.get(`${ORIGIN}/api/models`, () =>
      HttpResponse.json({ items: [], total: 0, page: 1, page_size: 20 }),
    ),
    http.get(`${ORIGIN}/api/models/missing`, () =>
      HttpResponse.json({ items: [], skipped_channels: 0, total: 0 }),
    ),
    http.post(`${ORIGIN}/api/vendors/operations/preview`, async ({ request }) => {
      const body = (await request.json()) as Record<string, unknown>
      onPreview?.(body)
      return HttpResponse.json({
        action: body.action,
        affected: 5,
        conflict: false,
        conflicts: [],
        expected_version: 'op-fingerprint-1',
        vendors: [
          { vendor_id: 1, version: 'v-openai-0001' },
          { vendor_id: 2, version: 'v-anthropic-0001' },
        ],
      })
    }),
    http.post(`${ORIGIN}/api/vendors/operations`, async ({ request }) => {
      const body = (await request.json()) as Record<string, unknown>
      onApply?.(body)
      if (applyStatus === 409) {
        return HttpResponse.json(
          { type: 'about:blank', title: 'Conflict', detail: 'stale fingerprint' },
          { status: 409, headers: { 'Content-Type': 'application/problem+json' } },
        )
      }
      return HttpResponse.json({ affected: 5 })
    }),
    http.delete(`${ORIGIN}/api/vendors/:id`, () => {
      if (deleteStatus !== 204) {
        const body: Record<string, unknown> = {
          type: 'https://newtg.dev/problems/vendor_referenced',
          title: 'Vendor is referenced',
          detail: 'the vendor is referenced by model metadata',
        }
        if (deleteReferenceCount !== undefined) {
          body.reference_count = deleteReferenceCount
        }
        return HttpResponse.json(body, {
          status: deleteStatus,
          headers: { 'Content-Type': 'application/problem+json' },
        })
      }
      return new HttpResponse(null, { status: 204 })
    }),
  )
}

function Harness() {
  const [search, setSearch] = useState<ModelCatalogSearch>({
    ...DEFAULT_MODEL_CATALOG_SEARCH,
    tab: 'vendors',
  })
  return (
    <ModelCatalogPage
      search={search}
      onSearchChange={(next) => {
        setSearch(next)
      }}
    />
  )
}

describe('vendors tab', () => {
  it('lists vendors with the model count and truncates the description', async () => {
    await renderWithProviders(<Harness />, ['common', 'admin'])

    expect(await screen.findByText('OpenAI')).toBeTruthy()
    expect(screen.getByText('Anthropic')).toBeTruthy()
    expect(screen.getByText('Frontier models')).toBeTruthy()
    expect(screen.getByText('3')).toBeTruthy()
  })

  it('requires preview before apply and sends the returned fingerprint', async () => {
    const previewBodies: Record<string, unknown>[] = []
    const applyBodies: Record<string, unknown>[] = []
    installHandlers({
      onPreview: (body) => previewBodies.push(body),
      onApply: (body) => applyBodies.push(body),
    })
    await renderWithProviders(<Harness />, ['common', 'admin'])
    const user = userEvent.setup()
    await screen.findByText('OpenAI')

    await user.click(screen.getByRole('checkbox', { name: 'Select all vendors on this page' }))
    await user.click(screen.getByRole('button', { name: 'Bulk operations' }))
    const wizard = await screen.findByRole('dialog', { name: 'Bulk operations' })

    // 默认 assign 需要模型；切换到 merge 后使用表格勾选的供应商。
    await user.selectOptions(within(wizard).getByLabelText('Operation'), 'merge')
    await user.selectOptions(within(wizard).getByLabelText('Target vendor'), '3')
    await user.click(within(wizard).getByRole('button', { name: 'Preview' }))

    expect(await within(wizard).findByText('Affected rows: 5')).toBeTruthy()
    expect(previewBodies).toEqual([
      { action: 'merge', vendor_ids: [1, 2], target_vendor_id: 3 },
    ])
    // apply 在预览之前不存在：两段不可跳过。
    expect(applyBodies).toEqual([])

    await user.click(within(wizard).getByRole('button', { name: 'Apply' }))
    await waitFor(() => {
      expect(applyBodies).toEqual([
        {
          action: 'merge',
          vendor_ids: [1, 2],
          target_vendor_id: 3,
          expected_version: 'op-fingerprint-1',
        },
      ])
    })
  })

  it('clears the preview and asks for a new one on a version conflict', async () => {
    installHandlers({ applyStatus: 409 })
    await renderWithProviders(<Harness />, ['common', 'admin'])
    const user = userEvent.setup()
    await screen.findByText('OpenAI')

    await user.click(screen.getByRole('checkbox', { name: 'Select all vendors on this page' }))
    await user.click(screen.getByRole('button', { name: 'Bulk operations' }))
    const wizard = await screen.findByRole('dialog', { name: 'Bulk operations' })
    await user.selectOptions(within(wizard).getByLabelText('Operation'), 'merge')
    await user.selectOptions(within(wizard).getByLabelText('Target vendor'), '3')
    await user.click(within(wizard).getByRole('button', { name: 'Preview' }))
    await within(wizard).findByText('Affected rows: 5')

    await user.click(within(wizard).getByRole('button', { name: 'Apply' }))
    expect(
      await within(wizard).findByText(/Versions changed or the fingerprint expired/),
    ).toBeTruthy()
    // 预览已作废，必须重新预览后才能再次应用。
    expect(within(wizard).getByRole('button', { name: 'Preview' })).toBeTruthy()
  })

  it('shows the reference count and routes a referenced vendor to merge', async () => {
    await renderWithProviders(<Harness />, ['common', 'admin'])
    const user = userEvent.setup()
    await screen.findByText('Anthropic')

    const [deleteAnthropic] = screen.getAllByRole('button', { name: 'Delete' }).slice(1)
    await user.click(deleteAnthropic!)
    const dialog = await screen.findByRole('alertdialog', { name: 'Delete vendor?' })
    expect(within(dialog).getByText(/referenced by 3 models/)).toBeTruthy()
    const deleteButton = within(dialog).getByRole('button', { name: 'Delete' }) as HTMLButtonElement
    expect(deleteButton.disabled).toBe(true)

    await user.click(within(dialog).getByRole('button', { name: 'Merge instead' }))
    const wizard = await screen.findByRole('dialog', { name: 'Bulk operations' })
    expect((within(wizard).getByLabelText('Operation') as HTMLSelectElement).value).toBe('merge')
    expect(within(wizard).getByText('Anthropic')).toBeTruthy()
  })

  it('uses the server reference_count when a delete races with a new reference', async () => {
    installHandlers({ deleteStatus: 409, deleteReferenceCount: 4 })
    await renderWithProviders(<Harness />, ['common', 'admin'])
    const user = userEvent.setup()
    await screen.findByText('OpenAI')

    // OpenAI 在列表里没有模型（删除按钮可用），但服务端在删除前一刻发现了 4 条引用。
    const [deleteOpenAI] = screen.getAllByRole('button', { name: 'Delete' })
    await user.click(deleteOpenAI!)
    const dialog = await screen.findByRole('alertdialog', { name: 'Delete vendor?' })
    await user.click(within(dialog).getByRole('button', { name: 'Delete' }))

    await waitFor(() => {
      expect(
        useToastStore
          .getState()
          .toasts.some((toast) => toast.message.includes('referenced by 4 models')),
      ).toBe(true)
    })
  })
})
