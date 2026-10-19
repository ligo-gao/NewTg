import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import {
  findRequest,
  installFetch,
  jsonResponse,
  problemResponse,
} from '@/features/tokens/test-utils'

import { ModelConfigPage } from './model-config-page'

const STATUS = {
  usable_groups: [
    {
      name: 'default',
      description: '',
      ratio: 100,
      models: ['gpt-4o-mini', 'gpt-4o'],
    },
    { name: 'pro', description: '', ratio: 150, models: ['gpt-4o', 'o3'] },
  ],
}

function selfSettings(defaultModel: string | null) {
  return {
    language: 'en',
    sidebar_modules: null,
    record_ip_log: false,
    default_model: defaultModel,
    updated_at: '2026-10-01T00:00:00Z',
  }
}

function token(overrides: Record<string, unknown> = {}) {
  return {
    id: 1,
    name: 'alpha',
    status: 'enabled',
    key_masked: `nkt-${'•'.repeat(24)}`,
    remain_quota: null,
    unlimited_quota: true,
    quota_percent: null,
    group: 'default',
    model_limits: null,
    allow_ips: null,
    auto_groups: [],
    cross_group_retry: false,
    created_at: '2026-10-01T00:00:00Z',
    updated_at: '2026-10-01T00:00:00Z',
    last_used_at: null,
    expires_at: null,
    ...overrides,
  }
}

function listResponse(items: unknown[]) {
  return { items, total: items.length, page: 1, page_size: 100 }
}

function install(handler: Parameters<typeof installFetch>[0]) {
  return installFetch((request) => {
    if (request.method === 'GET' && request.url.pathname === '/api/status') {
      return jsonResponse(STATUS)
    }
    if (request.method === 'GET' && request.url.pathname === '/api/self') {
      return jsonResponse(selfSettings('gpt-4o-mini'))
    }
    return handler(request)
  })
}

describe('ModelConfigPage', () => {
  it('saves the selected default model with only default_model and updated_at', async () => {
    const user = userEvent.setup()
    const requests = install((request) => {
      if (request.method === 'GET' && request.url.pathname === '/api/tokens') {
        return jsonResponse(listResponse([token()]))
      }
      if (request.method === 'PUT' && request.url.pathname === '/api/self/setting') {
        return jsonResponse(selfSettings('gpt-4o'))
      }
      return problemResponse(404, { type: 'about:blank', title: 'not found' })
    })

    await renderWithProviders(<ModelConfigPage />, ['account'])

    expect(await screen.findByText('Model Config')).toBeTruthy()
    expect((await screen.findByRole('radio', { name: 'gpt-4o' })) as HTMLInputElement)
      .toBeTruthy()

    await user.click(screen.getByRole('radio', { name: 'gpt-4o' }))
    await user.click(screen.getByRole('button', { name: 'Save' }))

    await waitFor(() => {
      expect(
        findRequest(requests, 'PUT', '/api/self/setting')?.body,
      ).toEqual({
        default_model: 'gpt-4o',
        updated_at: '2026-10-01T00:00:00Z',
      })
    })
    // 稀疏更新：绝不能带上语言/侧栏键覆盖其它偏好。
    const body = findRequest(requests, 'PUT', '/api/self/setting')?.body as
      | Record<string, unknown>
      | undefined
    expect(body === undefined ? undefined : 'language' in body).toBe(false)

    await waitFor(() => {
      expect(screen.getByTestId('default-model-current').textContent).toBe(
        'gpt-4o',
      )
    })
  })

  it('clears the default model by submitting an empty string', async () => {
    const user = userEvent.setup()
    const requests = install((request) => {
      if (request.method === 'GET' && request.url.pathname === '/api/tokens') {
        return jsonResponse(listResponse([token()]))
      }
      if (request.method === 'PUT' && request.url.pathname === '/api/self/setting') {
        return jsonResponse(selfSettings(null))
      }
      return problemResponse(404, { type: 'about:blank', title: 'not found' })
    })

    await renderWithProviders(<ModelConfigPage />, ['account'])
    await screen.findByRole('radio', { name: 'gpt-4o-mini' })

    await user.click(screen.getByRole('button', { name: 'Clear' }))

    await waitFor(() => {
      expect(findRequest(requests, 'PUT', '/api/self/setting')?.body).toEqual({
        default_model: '',
        updated_at: '2026-10-01T00:00:00Z',
      })
    })
    await waitFor(() => {
      expect(screen.getByTestId('default-model-current').textContent).toBe(
        'Not set (follows the current Playground selection)',
      )
    })
  })

  it('shows a field error and re-reads the server value on 422', async () => {
    const user = userEvent.setup()
    let selfReads = 0
    installFetch((request) => {
      if (request.method === 'GET' && request.url.pathname === '/api/status') {
        return jsonResponse(STATUS)
      }
      if (request.method === 'GET' && request.url.pathname === '/api/self') {
        selfReads += 1
        // 第一次读到 gpt-4o-mini；422 之后回读为 gpt-4o。
        return jsonResponse(selfSettings(selfReads === 1 ? 'gpt-4o-mini' : 'gpt-4o'))
      }
      if (request.method === 'GET' && request.url.pathname === '/api/tokens') {
        return jsonResponse(listResponse([token()]))
      }
      if (request.method === 'PUT' && request.url.pathname === '/api/self/setting') {
        return problemResponse(422, {
          type: 'https://newtg.dev/problems/default_model_invalid',
          title: 'user.default_model_invalid',
          fields: ['default_model'],
        })
      }
      return problemResponse(404, { type: 'about:blank', title: 'not found' })
    })

    await renderWithProviders(<ModelConfigPage />, ['account'])
    await screen.findByRole('radio', { name: 'gpt-4o-mini' })

    await user.click(screen.getByRole('radio', { name: 'gpt-4o' }))
    await user.click(screen.getByRole('button', { name: 'Save' }))

    expect((await screen.findByRole('alert')).textContent).toContain(
      'This model is not available in your groups',
    )
    await waitFor(() => {
      expect(screen.getByTestId('default-model-current').textContent).toBe(
        'gpt-4o',
      )
    })
    expect(selfReads).toBeGreaterThan(1)
  })

  it('edits a token model limit with a PATCH carrying the optimistic lock', async () => {
    const user = userEvent.setup()
    const requests = install((request) => {
      if (request.method === 'GET' && request.url.pathname === '/api/tokens') {
        return jsonResponse(listResponse([token()]))
      }
      if (request.method === 'PATCH' && request.url.pathname === '/api/tokens/1') {
        return jsonResponse(
          token({
            model_limits: ['gpt-4o'],
            updated_at: '2026-10-02T00:00:00Z',
          }),
        )
      }
      return problemResponse(404, { type: 'about:blank', title: 'not found' })
    })

    await renderWithProviders(<ModelConfigPage />, ['account'])
    const row = await screen.findByTestId('key-row-1')

    await user.click(within(row).getByRole('button', { name: 'Edit' }))
    await user.click(
      screen.getByRole('checkbox', { name: 'gpt-4o' }),
    )
    // 行内编辑器的 Save 在 DOM 中位于默认模型卡之后。
    const saveButtons = screen.getAllByRole('button', { name: 'Save' })
    await user.click(saveButtons[saveButtons.length - 1] as HTMLElement)

    await waitFor(() => {
      expect(findRequest(requests, 'PATCH', '/api/tokens/1')?.body).toEqual({
        model_limits: ['gpt-4o'],
        updated_at: '2026-10-01T00:00:00Z',
      })
    })
    await waitFor(() => {
      expect(within(screen.getByTestId('key-row-1')).getByText('1 models'))
        .toBeTruthy()
    })
  })

  it('reports partial batch failures and refreshes the list', async () => {
    const user = userEvent.setup()
    const tokens = [
      token({ id: 1, name: 'alpha' }),
      token({ id: 2, name: 'beta' }),
      token({ id: 3, name: 'gamma' }),
    ]
    const requests = install((request) => {
      if (request.method === 'GET' && request.url.pathname === '/api/tokens') {
        return jsonResponse(listResponse(tokens))
      }
      if (request.method === 'PATCH' && request.url.pathname === '/api/tokens/2') {
        return problemResponse(404, {
          type: 'about:blank',
          title: 'Token not found',
        })
      }
      if (request.method === 'PATCH' && request.url.pathname.startsWith('/api/tokens/')) {
        const id = Number(request.url.pathname.split('/').at(-1))
        return jsonResponse(
          token({ id, name: tokens[id - 1]?.name, model_limits: [] }),
        )
      }
      return problemResponse(404, { type: 'about:blank', title: 'not found' })
    })

    await renderWithProviders(<ModelConfigPage />, ['account'])
    await screen.findByText('alpha')

    await user.click(screen.getByRole('checkbox', { name: 'Select alpha' }))
    await user.click(screen.getByRole('checkbox', { name: 'Select beta' }))
    await user.click(screen.getByRole('checkbox', { name: 'Select gamma' }))
    await user.click(screen.getByRole('button', { name: /Set model limits in bulk/ }))

    const dialog = await screen.findByRole('dialog')
    await user.click(within(dialog).getByRole('button', { name: 'Apply' }))

    await waitFor(() => {
      expect(within(dialog).getByText('1 tokens failed')).toBeTruthy()
    })
    expect(within(dialog).getByText('beta')).toBeTruthy()
    expect(
      within(dialog).getByText('This token no longer exists'),
    ).toBeTruthy()
    expect(
      requests.filter(
        (request) =>
          request.method === 'PATCH' &&
          request.url.pathname.startsWith('/api/tokens/'),
      ),
    ).toHaveLength(3)
  })
})
