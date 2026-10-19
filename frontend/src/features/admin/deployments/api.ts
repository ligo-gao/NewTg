/**
 * 部署管理传输层（modules-08 §3.20 / UI §6.13，F-08-51～F-08-58）。
 *
 * 全部端点都走生成的 `apiClient`，因此错误统一是 `ApiError`、写请求自动带
 * CSRF，且响应形状与 `api/schema.d.ts` 在编译期绑定。唯一需要页面层控制的
 * 是 **Idempotency-Key**：中间件会在缺省时随机生成一个，但「同一用户操作
 * 的重试必须复用同一 key」只能由调用方决定——每个写函数都显式接收 key。
 */
import { apiClient } from '@/api/client'
import { requireData } from '@/api/result'
import type { components } from '@/api/schema'

export type DeployDeployment = components['schemas']['DeployDeployment']
export type DeployDeploymentList = components['schemas']['DeployDeploymentList']
export type DeployDeploymentDetail = components['schemas']['DeployDeploymentDetail']
export type DeploySettings = components['schemas']['DeploySettings']
export type DeployConnectionTestResult =
  components['schemas']['DeployConnectionTestResult']
export type DeployHardwareTypes = components['schemas']['DeployHardwareTypes']
export type DeployLocations = components['schemas']['DeployLocations']
export type DeployAvailableReplicas =
  components['schemas']['DeployAvailableReplicas']
export type DeployPriceEstimation =
  components['schemas']['DeployPriceEstimation']
export type DeployPriceEstimationRequest =
  components['schemas']['DeployPriceEstimationRequest']
export type DeployContainer = components['schemas']['DeployContainer']
export type DeployContainerList = components['schemas']['DeployContainerList']
export type DeployLogs = components['schemas']['DeployLogs']
export type CreateDeploymentRequest =
  components['schemas']['CreateDeploymentRequest']
export type UpdateDeploymentRequest =
  components['schemas']['UpdateDeploymentRequest']
export type DeployAction = components['schemas']['DeployAction']

export interface ListDeploymentsParams {
  keyword?: string
  status?: string
  p?: number
  page_size?: number
}

export async function listDeployments(
  params: ListDeploymentsParams,
  signal?: AbortSignal,
): Promise<DeployDeploymentList> {
  return requireData(
    await apiClient.GET('/api/deployments', {
      params: {
        query: {
          keyword: params.keyword,
          status: params.status,
          p: params.p,
          page_size: params.page_size,
        },
      },
      signal,
    }),
  )
}

export async function fetchDeploymentSettings(
  signal?: AbortSignal,
): Promise<DeploySettings> {
  return requireData(
    await apiClient.GET('/api/deployments/settings', { signal }),
  )
}

/**
 * 作用域化 PUT：enabled 必填；api_key 留空 = 不更新，clear_api_key 显式清除；
 * updated_at 是乐观锁版本。危险确认已由页面层的 DangerConfirm 完成。
 *
 * 契约未声明 `Idempotency-Key` 头（settings 写靠 `updated_at` 乐观锁防重），
 * 因此这里不显式传头；中间件仍会按写请求附带一个自动生成的 key。
 */
export async function saveDeploymentSettings(
  body: components['schemas']['UpdateDeploySettingsRequest'],
): Promise<DeploySettings> {
  return requireData(
    await apiClient.PUT('/api/deployments/settings', { body }),
  )
}

export async function testDeploymentConnection(
  candidateKey: string | undefined,
): Promise<DeployConnectionTestResult> {
  return requireData(
    await apiClient.POST('/api/deployments/settings/test-connection', {
      body: candidateKey === undefined ? {} : { api_key: candidateKey },
    }),
  )
}

export async function fetchHardwareTypes(
  signal?: AbortSignal,
): Promise<DeployHardwareTypes> {
  return requireData(
    await apiClient.GET('/api/deployments/hardware-types', { signal }),
  )
}

export async function fetchLocations(
  signal?: AbortSignal,
): Promise<DeployLocations> {
  return requireData(
    await apiClient.GET('/api/deployments/locations', { signal }),
  )
}

export async function fetchAvailableReplicas(
  hardwareId: number,
  gpuCount: number,
  signal?: AbortSignal,
): Promise<DeployAvailableReplicas> {
  return requireData(
    await apiClient.GET('/api/deployments/available-replicas', {
      params: { query: { hardware_id: hardwareId, gpu_count: gpuCount } },
      signal,
    }),
  )
}

export async function estimateDeploymentPrice(
  body: DeployPriceEstimationRequest,
  signal?: AbortSignal,
): Promise<DeployPriceEstimation> {
  return requireData(
    await apiClient.POST('/api/deployments/price-estimation', {
      body,
      signal,
    }),
  )
}

export async function checkDeploymentName(
  name: string,
  signal?: AbortSignal,
): Promise<components['schemas']['DeployNameAvailability']> {
  return requireData(
    await apiClient.GET('/api/deployments/check-name', {
      params: { query: { name } },
      signal,
    }),
  )
}

export async function createDeployment(
  body: CreateDeploymentRequest,
  idempotencyKey: string,
): Promise<DeployAction> {
  return requireData(
    await apiClient.POST('/api/deployments', {
      params: { header: { 'Idempotency-Key': idempotencyKey } },
      body,
    }),
  )
}

export async function fetchDeployment(
  id: string,
  signal?: AbortSignal,
): Promise<DeployDeploymentDetail> {
  return requireData(
    await apiClient.GET('/api/deployments/{id}', {
      params: { path: { id } },
      signal,
    }),
  )
}

export async function fetchDeploymentContainers(
  id: string,
  signal?: AbortSignal,
): Promise<DeployContainerList> {
  return requireData(
    await apiClient.GET('/api/deployments/{id}/containers', {
      params: { path: { id } },
      signal,
    }),
  )
}

export async function fetchDeploymentContainer(
  id: string,
  containerId: string,
  signal?: AbortSignal,
): Promise<DeployContainer> {
  return requireData(
    await apiClient.GET('/api/deployments/{id}/containers/{container_id}', {
      params: { path: { id, container_id: containerId } },
      signal,
    }),
  )
}

export interface DeploymentLogsParams {
  containerId: string
  level?: string
  stream?: string
  pageToken?: string
  limit?: number
}

export async function fetchDeploymentLogs(
  id: string,
  params: DeploymentLogsParams,
  signal?: AbortSignal,
): Promise<DeployLogs> {
  return requireData(
    await apiClient.GET('/api/deployments/{id}/logs', {
      params: {
        path: { id },
        query: {
          container_id: params.containerId,
          level: params.level,
          stream: params.stream,
          page_token: params.pageToken,
          limit: params.limit,
        },
      },
      signal,
    }),
  )
}

export async function updateDeployment(
  id: string,
  body: UpdateDeploymentRequest,
  idempotencyKey: string,
): Promise<DeployAction> {
  return requireData(
    await apiClient.PATCH('/api/deployments/{id}', {
      params: { path: { id }, header: { 'Idempotency-Key': idempotencyKey } },
      body,
    }),
  )
}

export async function renameDeployment(
  id: string,
  name: string,
  idempotencyKey: string,
): Promise<DeployAction> {
  return requireData(
    await apiClient.PATCH('/api/deployments/{id}/name', {
      params: { path: { id }, header: { 'Idempotency-Key': idempotencyKey } },
      body: { name },
    }),
  )
}

export async function extendDeployment(
  id: string,
  durationHours: number,
  idempotencyKey: string,
): Promise<DeployAction> {
  return requireData(
    await apiClient.POST('/api/deployments/{id}/extend', {
      params: { path: { id }, header: { 'Idempotency-Key': idempotencyKey } },
      body: { duration_hours: durationHours },
    }),
  )
}

export async function terminateDeployment(
  id: string,
  idempotencyKey: string,
): Promise<DeployAction> {
  return requireData(
    await apiClient.DELETE('/api/deployments/{id}', {
      params: { path: { id }, header: { 'Idempotency-Key': idempotencyKey } },
    }),
  )
}

/** 每次用户提交生成一次；重试同一意图时由页面层复用。 */
export function createIdempotencyKey(): string {
  if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
    return crypto.randomUUID()
  }
  return `${Date.now()}-${Math.random().toString(16).slice(2)}`
}
