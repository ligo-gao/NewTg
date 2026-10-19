/**
 * 三协议代码样例（F-09-27，净室重写）。
 *
 * 硬约束：
 * - `base_url` 只来自 `/api/status` 的 `site.server_address`；
 * - 模型名来自当前详情；
 * - 密钥永远是占位符 `<YOUR_TOKEN>`，任何形似真实令牌的串都不进入前端。
 */

export type SampleTab = 'openai' | 'anthropic' | 'gemini'

export interface SampleTarget {
  /** 站点配置的绝对地址；未配置时为空串。 */
  baseUrl: string
  configured: boolean
}

export function sampleTarget(serverAddress: string | undefined): SampleTarget {
  const value = (serverAddress ?? '').trim()
  return { baseUrl: value, configured: value !== '' }
}

function openaiBase(baseUrl: string): string {
  if (baseUrl === '') return '/v1'
  return `${baseUrl.replace(/\/+$/, '')}/v1`
}

function anthropicBase(baseUrl: string): string {
  if (baseUrl === '') return ''
  return baseUrl.replace(/\/+$/, '')
}

export function sampleCode(
  tab: SampleTab,
  modelId: string,
  target: SampleTarget,
): string {
  switch (tab) {
    case 'openai':
      return `from openai import OpenAI

client = OpenAI(
    base_url="${openaiBase(target.baseUrl)}",
    api_key="<YOUR_TOKEN>",
)
response = client.chat.completions.create(
    model="${modelId}",
    messages=[{"role": "user", "content": "Hello"}],
)
print(response.choices[0].message.content)`
    case 'anthropic':
      return `from anthropic import Anthropic

client = Anthropic(
    base_url="${anthropicBase(target.baseUrl)}",
    api_key="<YOUR_TOKEN>",
)
message = client.messages.create(
    model="${modelId}",
    max_tokens=1024,
    messages=[{"role": "user", "content": "Hello"}],
)
print(message.content[0].text)`
    case 'gemini':
      return `from google import genai

client = genai.Client(
    api_key="<YOUR_TOKEN>",
    http_options={"base_url": "${target.baseUrl}"},
)
response = client.models.generate_content(
    model="${modelId}",
    contents="Hello",
)
print(response.text)`
  }
}
