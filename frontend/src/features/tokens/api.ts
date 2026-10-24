import { apiClient } from '@/api/client'
import { requireData } from '@/api/result'
import type { components } from '@/api/schema'

/**
 * Token management API (modules-02 F-02-01…F-02-17).
 *
 * Every call goes through the frozen contract client (`api/client.ts`), so
 * CSRF, `Idempotency-Key` generation and RFC 7807 normalization are applied
 * once. The generated `paths` types are the only source of request/response
 * shapes; nothing here re-declares a DTO.
 */
export type Token = components['schemas']['Token']
export type TokenStatus = components['schemas']['TokenStatus']
export type CreateTokenRequest = components['schemas']['CreateTokenRequest']
export type CreatedToken = components['schemas']['CreatedToken']
export type UpdateTokenRequest = components['schemas']['UpdateTokenRequest']
export type TokenBatchResponse = components['schemas']['TokenBatchResponse']
export type TokenAutoGroupsResponse =
  components['schemas']['TokenAutoGroupsResponse']

/**
 * The generated `TokenList` intersects `Page` (whose `items: unknown[]`) with
 * `{items?: Token[]}`, which TypeScript collapses back to `unknown[]`. This
 * projection restores the element type without re-declaring the wire fields.
 */
export interface TokenList {
  items: Token[]
  total: number
  page: number
  page_size: number
}

export type TokenQuotaFilter = 'normal' | 'low' | 'exhausted' | 'unlimited'

export interface TokenListParams {
  page: number
  pageSize: number
  keyword?: string
  status?: TokenStatus
  group?: string
  quota?: TokenQuotaFilter
}

export async function listTokens(params: TokenListParams): Promise<TokenList> {
  const query: {
    p: number
    page_size: number
    keyword?: string
    status?: TokenStatus
    group?: string
    quota?: TokenQuotaFilter
  } = { p: params.page, page_size: params.pageSize }
  if (params.keyword !== undefined && params.keyword !== '') {
    query.keyword = params.keyword
  }
  if (params.status !== undefined) query.status = params.status
  if (params.group !== undefined && params.group !== '') {
    query.group = params.group
  }
  if (params.quota !== undefined) query.quota = params.quota
  const data = requireData(await apiClient.GET('/api/tokens', { params: { query } }))
  return {
    items: (data.items ?? []) as Token[],
    total: data.total,
    page: data.page,
    page_size: data.page_size,
  }
}

export async function getToken(id: number): Promise<Token> {
  return requireData(
    await apiClient.GET('/api/tokens/{id}', { params: { path: { id } } }),
  )
}

/**
 * `idempotencyKey` is minted per create intent, not per attempt: retrying the
 * same submission replays the first result instead of creating duplicates.
 */
export async function createTokens(
  body: CreateTokenRequest,
  idempotencyKey: string,
): Promise<CreatedToken[]> {
  const result = await apiClient.POST('/api/tokens', {
    body,
    params: { header: { 'Idempotency-Key': idempotencyKey } },
  })
  return requireData(result).created
}

/**
 * PATCH is optimistic-locked: `updated_at` must equal the row version the user
 * loaded, otherwise the server answers 409 (modules-02 §2.5).
 */
export async function updateToken(
  id: number,
  body: UpdateTokenRequest,
): Promise<Token> {
  return requireData(
    await apiClient.PATCH('/api/tokens/{id}', {
      body,
      params: { path: { id } },
    }),
  )
}

/** DELETE optionally carries the version; a mismatch answers 409. */
export async function deleteToken(id: number, updatedAt: string): Promise<void> {
  await apiClient.DELETE('/api/tokens/{id}', {
    params: { path: { id }, query: { updated_at: updatedAt } },
  })
}

export type BatchTokenAction = 'enable' | 'disable' | 'delete'

export async function batchTokens(
  action: BatchTokenAction,
  ids: number[],
): Promise<TokenBatchResponse> {
  const body = { ids }
  if (action === 'enable') {
    return requireData(await apiClient.POST('/api/tokens:batch-enable', { body }))
  }
  if (action === 'disable') {
    return requireData(await apiClient.POST('/api/tokens:batch-disable', { body }))
  }
  return requireData(await apiClient.POST('/api/tokens:batch-delete', { body }))
}

export async function setTokenEnabled(id: number, enabled: boolean): Promise<Token> {
  if (enabled) {
    return requireData(
      await apiClient.POST('/api/tokens/{id}:enable', { params: { path: { id } } }),
    )
  }
  return requireData(
    await apiClient.POST('/api/tokens/{id}:disable', { params: { path: { id } } }),
  )
}

export async function resetTokenKey(
  id: number,
): Promise<{ key: string; key_masked: string }> {
  return requireData(
    await apiClient.POST('/api/tokens/{id}:reset-key', {
      params: { path: { id } },
    }),
  )
}

export async function listAutoGroups(): Promise<TokenAutoGroupsResponse> {
  return requireData(await apiClient.GET('/api/token/auto-groups'))
}

/** Masked key shape: `sk-` + 24 `•` (legacy `nkt-` tokens remain valid). */
export const MASKED_KEY = `sk-${'•'.repeat(24)}`

export const MAX_BATCH = 100
export const MAX_QUANTITY = 100
