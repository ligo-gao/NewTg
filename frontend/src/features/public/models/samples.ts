/**
 * 「立即调用」的请求样例（模型库「立即调用」弹框）。
 *
 * 约定：
 * - 只描述本站的 OpenAI 兼容端点，密钥永远是占位符 `<YOUR_TOKEN>`，
 *   任何形似真实令牌的串都不进入前端；
 * - base URL 只来自 `site.server_address`，未配置时用相对 `/v1`；
 * - 六种调用类型各有固定 path 与请求体，只有 chat 允许流式；
 * - Rerank 没有 OpenAI SDK 方法，Python 用 requests、Node 用 fetch。
 */

import {
  openaiBase,
  type SampleTarget,
} from '@/features/public/pricing/code-samples'

import type { CallKind } from './performance'

export type CallTab = 'curl' | 'python' | 'node'

export interface CallSpec {
  kind: CallKind
  method: 'POST'
  path: string
  body: Record<string, unknown>
  stream: boolean
  response: string
}

const TOKEN = '<YOUR_TOKEN>'

const CHAT_PROMPT = 'Explain what an API gateway does in one sentence.'
const EMBEDDING_INPUT = 'An API gateway unifies many providers behind one endpoint.'
const IMAGE_PROMPT = 'A top-down view of an old map in fine-line print style'
const SPEECH_TEXT = 'The gateway is ready for your first request.'
const RERANK_QUERY = 'How do I reset my password?'
const RERANK_DOCUMENTS = [
  'Open Settings and click Reset password.',
  'Where can I download invoices?',
  'How do I change my email address?',
]

const PATHS: Record<CallKind, string> = {
  chat: '/chat/completions',
  embedding: '/embeddings',
  image: '/images/generations',
  speech: '/audio/speech',
  transcription: '/audio/transcriptions',
  rerank: '/rerank',
}

function bodyOf(
  kind: CallKind,
  modelId: string,
  stream: boolean,
): Record<string, unknown> {
  switch (kind) {
    case 'chat':
      return {
        model: modelId,
        messages: [{ role: 'user', content: CHAT_PROMPT }],
        ...(stream ? { stream: true } : {}),
      }
    case 'embedding':
      return { model: modelId, input: EMBEDDING_INPUT }
    case 'image':
      return {
        model: modelId,
        prompt: IMAGE_PROMPT,
        size: '1024x1024',
        n: 1,
      }
    case 'speech':
      return {
        model: modelId,
        input: SPEECH_TEXT,
        voice: 'alloy',
        response_format: 'mp3',
      }
    case 'transcription':
      return {
        file: 'meeting.mp3',
        model: modelId,
        response_format: 'verbose_json',
      }
    case 'rerank':
      return {
        model: modelId,
        query: RERANK_QUERY,
        documents: [...RERANK_DOCUMENTS],
        top_n: 2,
      }
  }
}

function chatResponse(modelId: string, stream: boolean): string {
  if (stream) {
    return `data: {"id":"chatcmpl-9f2c","choices":[{"delta":{"role":"assistant","content":"An"}}]}

data: {"id":"chatcmpl-9f2c","choices":[{"delta":{"content":" API gateway"}}]}

data: {"id":"chatcmpl-9f2c","choices":[{"delta":{"content":" unifies providers."}}]}

data: [DONE]`
  }
  return `{
  "id": "chatcmpl-9f2c",
  "object": "chat.completion",
  "model": "${modelId}",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": "An API gateway unifies many providers behind one endpoint."
      },
      "finish_reason": "stop"
    }
  ],
  "usage": {
    "prompt_tokens": 18,
    "completion_tokens": 46,
    "total_tokens": 64
  }
}`
}

function responseOf(
  kind: CallKind,
  modelId: string,
  stream: boolean,
): string {
  switch (kind) {
    case 'chat':
      return chatResponse(modelId, stream)
    case 'embedding':
      return `{
  "object": "list",
  "data": [
    {
      "object": "embedding",
      "index": 0,
      "embedding": [0.0023, -0.0141, 0.0087, ...]
    }
  ],
  "model": "${modelId}",
  "usage": {
    "prompt_tokens": 12,
    "total_tokens": 12
  }
}`
    case 'image':
      return `{
  "created": 1761264000,
  "data": [
    {
      "revised_prompt": "A top-down view of an old map, fine-line print style...",
      "url": "https://cdn.example.com/generated/9f2c.png"
    }
  ]
}`
    case 'speech':
      return `HTTP/2 200
content-type: audio/mpeg
content-length: 28416

# The response body is audio binary, written to speech.mp3`
    case 'transcription':
      return `{
  "task": "transcribe",
  "language": "en",
  "duration": 42.6,
  "text": "The gateway is ready for your first request.",
  "segments": [
    {"id": 0, "start": 0.0, "end": 4.2, "text": "The gateway is ready"}
  ]
}`
    case 'rerank':
      return `{
  "results": [
    {"index": 0, "relevance_score": 0.94},
    {"index": 2, "relevance_score": 0.41}
  ],
  "usage": {
    "total_tokens": 42
  }
}`
  }
}

/**
 * 描述一次调用的方法与请求体。`stream` 只有 chat 会真正生效；
 * 其它类型即使传入 true 也保持非流式，示例里不会出现 stream 字段。
 */
export function callSpec(
  kind: CallKind,
  modelId: string,
  stream: boolean,
): CallSpec {
  const streaming = kind === 'chat' && stream
  return {
    kind,
    method: 'POST',
    path: PATHS[kind],
    body: bodyOf(kind, modelId, streaming),
    stream: streaming,
    response: responseOf(kind, modelId, streaming),
  }
}

/** 单引号包裹的 shell 字符串里，字面引号必须写成 `'\''`。 */
function escapeShellSingleQuotes(value: string): string {
  return value.replace(/'/g, "'\\''")
}

/**
 * 把值安全嵌入 Python / JavaScript 的双引号字面量。模型 ID 由上游目录
 * 提供，可能带引号或反斜杠；不转义会生成语法错误的示例代码。
 */
function escapeDoubleQuoted(value: string): string {
  return value
    .replace(/\\/g, '\\\\')
    .replace(/"/g, '\\"')
    .replace(/\n/g, '\\n')
}

function curlSample(spec: CallSpec, target: SampleTarget): string {
  const url = `${openaiBase(target.baseUrl)}${spec.path}`
  const auth = `  -H "Authorization: Bearer ${TOKEN}" \\`

  if (spec.kind === 'transcription') {
    return [
      `curl ${url} \\`,
      auth,
      `  -F file=@${String(spec.body.file)} \\`,
      `  -F model="${String(spec.body.model)}" \\`,
      `  -F response_format="${String(spec.body.response_format)}"`,
    ].join('\n')
  }

  const json = escapeShellSingleQuotes(JSON.stringify(spec.body, null, 2))
  const payload = `  -d '${json}'`
  return [
    `curl ${url} \\`,
    auth,
    '  -H "Content-Type: application/json" \\',
    spec.kind === 'speech' ? `${payload} \\` : payload,
    ...(spec.kind === 'speech' ? ['  --output speech.mp3'] : []),
  ].join('\n')
}

function pythonSample(spec: CallSpec, target: SampleTarget): string {
  const modelId = escapeDoubleQuoted(String(spec.body.model))
  const base = openaiBase(target.baseUrl)

  switch (spec.kind) {
    case 'chat': {
      const head = `from openai import OpenAI

client = OpenAI(
    base_url="${base}",
    api_key="${TOKEN}",
)

resp = client.chat.completions.create(
    model="${modelId}",
    messages=[
        {"role": "user", "content": "${CHAT_PROMPT}"},
    ],`
      return spec.stream
        ? `${head}
    stream=True,
)

for chunk in resp:
    delta = chunk.choices[0].delta.content
    if delta:
        print(delta, end="", flush=True)`
        : `${head}
)

print(resp.choices[0].message.content)`
    }
    case 'embedding':
      return `from openai import OpenAI

client = OpenAI(
    base_url="${base}",
    api_key="${TOKEN}",
)

resp = client.embeddings.create(
    model="${modelId}",
    input="${EMBEDDING_INPUT}",
)
print(len(resp.data[0].embedding))`
    case 'image':
      return `from openai import OpenAI

client = OpenAI(
    base_url="${base}",
    api_key="${TOKEN}",
)

resp = client.images.generate(
    model="${modelId}",
    prompt="${IMAGE_PROMPT}",
    size="1024x1024",
    n=1,
)
print(resp.data[0].url)`
    case 'speech':
      return `from openai import OpenAI

client = OpenAI(
    base_url="${base}",
    api_key="${TOKEN}",
)

with client.audio.speech.with_streaming_response.create(
    model="${modelId}",
    voice="alloy",
    input="${SPEECH_TEXT}",
) as resp:
    resp.stream_to_file("speech.mp3")`
    case 'transcription':
      return `from openai import OpenAI

client = OpenAI(
    base_url="${base}",
    api_key="${TOKEN}",
)

with open("meeting.mp3", "rb") as audio:
    resp = client.audio.transcriptions.create(
        model="${modelId}",
        file=audio,
        response_format="verbose_json",
    )

print(resp.text)`
    case 'rerank':
      return `import requests

resp = requests.post(
    "${base}${spec.path}",
    headers={"Authorization": "Bearer ${TOKEN}"},
    json={
        "model": "${modelId}",
        "query": "${RERANK_QUERY}",
        "documents": [
${RERANK_DOCUMENTS.map((doc) => `            "${doc}"`).join(',\n')}
        ],
        "top_n": 2,
    },
)
print(resp.json()["results"])`
  }
}

function nodeSample(spec: CallSpec, target: SampleTarget): string {
  const modelId = escapeDoubleQuoted(String(spec.body.model))
  const base = openaiBase(target.baseUrl)
  const client = `import OpenAI from "openai";

const client = new OpenAI({
  baseURL: "${base}",
  apiKey: "${TOKEN}",
});`

  switch (spec.kind) {
    case 'chat': {
      const call = `const resp = await client.chat.completions.create({
  model: "${modelId}",
  messages: [{ role: "user", content: "${CHAT_PROMPT}" }],${spec.stream ? '\n  stream: true,' : ''}
});`
      const tail = spec.stream
        ? `
for await (const chunk of resp) {
  const delta = chunk.choices[0]?.delta?.content;
  if (delta) process.stdout.write(delta);
}`
        : `
console.log(resp.choices[0].message.content);`
      return `${client}

${call}${tail}`
    }
    case 'embedding':
      return `${client}

const resp = await client.embeddings.create({
  model: "${modelId}",
  input: "${EMBEDDING_INPUT}",
});
console.log(resp.data[0].embedding.length);`
    case 'image':
      return `${client}

const resp = await client.images.generate({
  model: "${modelId}",
  prompt: "${IMAGE_PROMPT}",
  size: "1024x1024",
  n: 1,
});
console.log(resp.data[0].url);`
    case 'speech':
      return `import fs from "node:fs";
import OpenAI from "openai";

const client = new OpenAI({
  baseURL: "${base}",
  apiKey: "${TOKEN}",
});

const resp = await client.audio.speech.create({
  model: "${modelId}",
  voice: "alloy",
  input: "${SPEECH_TEXT}",
});
fs.writeFileSync("speech.mp3", Buffer.from(await resp.arrayBuffer()));`
    case 'transcription':
      return `import fs from "node:fs";
import OpenAI from "openai";

const client = new OpenAI({
  baseURL: "${base}",
  apiKey: "${TOKEN}",
});

const resp = await client.audio.transcriptions.create({
  model: "${modelId}",
  file: fs.createReadStream("meeting.mp3"),
  response_format: "verbose_json",
});
console.log(resp.text);`
    case 'rerank':
      return `const resp = await fetch("${base}${spec.path}", {
  method: "POST",
  headers: {
    Authorization: "Bearer ${TOKEN}",
    "Content-Type": "application/json",
  },
  body: JSON.stringify({
    model: "${modelId}",
    query: "${RERANK_QUERY}",
    documents: [
${RERANK_DOCUMENTS.map((doc) => `      "${doc}"`).join(',\n')}
    ],
    top_n: 2,
  }),
});
console.log((await resp.json()).results);`
  }
}

/** 按语言生成可复制的调用样例；base URL 只取自 `target`。 */
export function callSample(
  tab: CallTab,
  spec: CallSpec,
  target: SampleTarget,
): string {
  switch (tab) {
    case 'curl':
      return curlSample(spec, target)
    case 'python':
      return pythonSample(spec, target)
    case 'node':
      return nodeSample(spec, target)
  }
}
