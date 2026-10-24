/**
 * 上游用量对账传输层（OpenAPI 契约已合并，类型来自 `api/schema.d.ts`）。
 *
 * 全部端点都走生成的 `apiClient`，错误统一是 `ApiError`，写请求自动带
 * CSRF，响应形状在编译期与契约绑定。读请求接受可选的 `AbortSignal`，写
 * 请求显式接收 **Idempotency-Key**：中间件只在缺省时随机生成，而「同一
 * 用户操作的重试必须复用同一 key」只能由调用方决定。
 *
 * 快照写入按契约拆成 `UpstreamSnapshotWrite`：`account_id` 在外层信封，
 * `source`/`entered_by`/`entered_at`/`raw`/时间戳等服务端字段不得提交。
 */
import { apiClient } from '@/api/client'
import { requireData } from '@/api/result'
import type { components } from '@/api/schema'

export type UpstreamAccount = components['schemas']['UpstreamAccount']
export type UpstreamAccountList = components['schemas']['UpstreamAccountList']
export type UpstreamAccountCreate =
  components['schemas']['UpstreamAccountCreate']
export type UpstreamAccountUpdate =
  components['schemas']['UpstreamAccountUpdate']
export type UpstreamSnapshot = components['schemas']['UpstreamSnapshot']
export type UpstreamSnapshotWrite =
  components['schemas']['UpstreamSnapshotWrite']
export type UpstreamSnapshotList =
  components['schemas']['UpstreamSnapshotList']
export type UpstreamSnapshotUpsert =
  components['schemas']['UpstreamSnapshotUpsert']
export type UpstreamSnapshotUpsertResult =
  components['schemas']['UpstreamSnapshotUpsertResult']
export type ReconcileRun = components['schemas']['ReconcileRun']
export type ReconcileRunList = components['schemas']['ReconcileRunList']
export type ReconcileItem = components['schemas']['ReconcileItem']
export type ReconcileItemList = components['schemas']['ReconcileItemList']
export type ReconcileSummary = components['schemas']['ReconcileSummary']
export type ReconcileRunRequest =
  components['schemas']['ReconcileRunRequest']
export type ReconcileRunResult = components['schemas']['ReconcileRunResult']
export type ReconcileAckRequest =
  components['schemas']['ReconcileAckRequest']
export type ReconcileSeverity = ReconcileItem['severity']
export type ReconcileItemStatus = ReconcileItem['status']

export interface ListAccountsParams {
  status?: 1 | 2
  p?: number
  page_size?: number
}

export interface ListSnapshotsParams {
  accountId: number
  from?: string
  to?: string
}

export interface ListRunsParams {
  accountId?: number
  p?: number
  page_size?: number
}

export interface ListItemsParams {
  severity?: ReconcileSeverity
  status?: ReconcileItemStatus
  p?: number
  page_size?: number
}

export async function listAccounts(
  params: ListAccountsParams,
  signal?: AbortSignal,
): Promise<UpstreamAccountList> {
  return requireData(
    await apiClient.GET('/api/upstream-accounts', {
      params: {
        query: {
          status: params.status,
          p: params.p,
          page_size: params.page_size,
        },
      },
      signal,
    }),
  )
}

export async function listSnapshots(
  params: ListSnapshotsParams,
  signal?: AbortSignal,
): Promise<UpstreamSnapshotList> {
  return requireData(
    await apiClient.GET('/api/upstream-snapshots', {
      params: {
        query: {
          account_id: params.accountId,
          from: params.from,
          to: params.to,
        },
      },
      signal,
    }),
  )
}

export async function listRuns(
  params: ListRunsParams,
  signal?: AbortSignal,
): Promise<ReconcileRunList> {
  return requireData(
    await apiClient.GET('/api/upstream-reconciliations/runs', {
      params: {
        query: {
          account_id: params.accountId,
          p: params.p,
          page_size: params.page_size,
        },
      },
      signal,
    }),
  )
}

export async function listItems(
  runId: number,
  params: ListItemsParams,
  signal?: AbortSignal,
): Promise<ReconcileItemList> {
  return requireData(
    await apiClient.GET('/api/upstream-reconciliations/runs/{id}/items', {
      params: {
        path: { id: runId },
        query: {
          severity: params.severity,
          status: params.status,
          p: params.p,
          page_size: params.page_size,
        },
      },
      signal,
    }),
  )
}

export async function getSummary(
  runId: number,
  signal?: AbortSignal,
): Promise<ReconcileSummary> {
  return requireData(
    await apiClient.GET('/api/upstream-reconciliations/summary', {
      params: { query: { run_id: runId } },
      signal,
    }),
  )
}

export async function createAccount(
  body: UpstreamAccountCreate,
  idempotencyKey: string,
): Promise<UpstreamAccount> {
  return requireData(
    await apiClient.POST('/api/upstream-accounts', {
      params: { header: { 'Idempotency-Key': idempotencyKey } },
      body,
    }),
  )
}

export async function updateAccount(
  id: number,
  body: UpstreamAccountUpdate,
  idempotencyKey: string,
): Promise<UpstreamAccount> {
  return requireData(
    await apiClient.PATCH('/api/upstream-accounts/{id}', {
      params: { path: { id }, header: { 'Idempotency-Key': idempotencyKey } },
      body,
    }),
  )
}

export async function deleteAccount(
  id: number,
  idempotencyKey: string,
): Promise<void> {
  // 204 没有响应体；仍被引用的账号由服务端以 409 拒绝。
  await apiClient.DELETE('/api/upstream-accounts/{id}', {
    params: { path: { id }, header: { 'Idempotency-Key': idempotencyKey } },
  })
}

export async function upsertSnapshots(
  body: UpstreamSnapshotUpsert,
  idempotencyKey: string,
): Promise<UpstreamSnapshotUpsertResult> {
  return requireData(
    await apiClient.PUT('/api/upstream-snapshots', {
      params: { header: { 'Idempotency-Key': idempotencyKey } },
      body,
    }),
  )
}

export async function triggerRun(
  body: ReconcileRunRequest,
  idempotencyKey: string,
): Promise<ReconcileRunResult> {
  return requireData(
    await apiClient.POST('/api/upstream-reconciliations/run', {
      params: { header: { 'Idempotency-Key': idempotencyKey } },
      body,
    }),
  )
}

export async function ackItem(
  id: number,
  body: ReconcileAckRequest,
  idempotencyKey: string,
): Promise<void> {
  await apiClient.POST('/api/upstream-reconciliations/items/{id}/ack', {
    params: { path: { id }, header: { 'Idempotency-Key': idempotencyKey } },
    body,
  })
}

/** 每次用户提交生成一次；重试同一意图时由页面层复用。 */
export function createIdempotencyKey(): string {
  if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
    return crypto.randomUUID()
  }
  return `${Date.now()}-${Math.random().toString(16).slice(2)}`
}
