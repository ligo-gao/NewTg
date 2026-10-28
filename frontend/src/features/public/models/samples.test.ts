import { describe, expect, it } from 'vitest'

import {
  openaiBase,
  sampleTarget,
} from '@/features/public/pricing/code-samples'

import type { CallKind } from './performance'
import { callSample, callSpec, type CallTab } from './samples'

const MODEL_ID = 'openai/gpt-5'
const CONFIGURED = sampleTarget('https://gateway.example.com/')
const FALLBACK = sampleTarget(undefined)

const PATHS: ReadonlyArray<readonly [CallKind, string]> = [
  ['chat', '/chat/completions'],
  ['embedding', '/embeddings'],
  ['image', '/images/generations'],
  ['speech', '/audio/speech'],
  ['transcription', '/audio/transcriptions'],
  ['rerank', '/rerank'],
]

const TABS: readonly CallTab[] = ['curl', 'python', 'node']

describe('callSpec', () => {
  it('maps each kind to its OpenAI-compatible path', () => {
    for (const [kind, path] of PATHS) {
      const spec = callSpec(kind, MODEL_ID, false)
      expect(spec.kind).toBe(kind)
      expect(spec.method).toBe('POST')
      expect(spec.path).toBe(path)
      expect(spec.body.model).toBe(MODEL_ID)
      expect(spec.response.length).toBeGreaterThan(0)
    }
  })

  it('adds stream: true only for chat', () => {
    const streaming = callSpec('chat', MODEL_ID, true)
    expect(streaming.stream).toBe(true)
    expect(streaming.body.stream).toBe(true)

    const plain = callSpec('chat', MODEL_ID, false)
    expect(plain.stream).toBe(false)
    expect('stream' in plain.body).toBe(false)

    for (const [kind] of PATHS) {
      if (kind === 'chat') continue
      const spec = callSpec(kind, MODEL_ID, true)
      expect(spec.stream).toBe(false)
      expect('stream' in spec.body).toBe(false)
    }
  })

  it('uses a multipart body for transcription', () => {
    const spec = callSpec('transcription', MODEL_ID, false)
    expect(spec.body).toMatchObject({
      file: 'meeting.mp3',
      model: MODEL_ID,
      response_format: 'verbose_json',
    })
  })
})

describe('callSample', () => {
  it('uses the configured base url and falls back to /v1', () => {
    const spec = callSpec('chat', MODEL_ID, false)

    expect(openaiBase(CONFIGURED.baseUrl)).toBe('https://gateway.example.com/v1')
    expect(callSample('curl', spec, CONFIGURED)).toContain(
      'curl https://gateway.example.com/v1/chat/completions',
    )
    expect(callSample('python', spec, CONFIGURED)).toContain(
      'base_url="https://gateway.example.com/v1"',
    )
    expect(callSample('node', spec, CONFIGURED)).toContain(
      'baseURL: "https://gateway.example.com/v1"',
    )

    expect(FALLBACK.configured).toBe(false)
    expect(callSample('curl', spec, FALLBACK)).toContain(
      'curl /v1/chat/completions',
    )
    expect(callSample('python', spec, FALLBACK)).toContain('base_url="/v1"')
  })

  it('always uses <YOUR_TOKEN> and never a sk- literal', () => {
    for (const [kind] of PATHS) {
      const spec = callSpec(kind, MODEL_ID, false)
      for (const tab of TABS) {
        const sample = callSample(tab, spec, CONFIGURED)
        expect(sample).toContain('<YOUR_TOKEN>')
        expect(sample).not.toMatch(/\bsk-[A-Za-z0-9]/)
      }
    }
  })

  it('uses multipart -F for transcription cURL', () => {
    const sample = callSample(
      'curl',
      callSpec('transcription', MODEL_ID, false),
      CONFIGURED,
    )
    expect(sample).toContain('-F file=@meeting.mp3')
    expect(sample).toContain(`-F model="${MODEL_ID}"`)
    expect(sample).toContain('-F response_format="verbose_json"')
    expect(sample).not.toContain('Content-Type: application/json')
  })

  it('escapes single quotes in the JSON body for cURL', () => {
    const spec = callSpec('chat', "vendor/it's-model", false)
    const sample = callSample('curl', spec, CONFIGURED)

    // Inside a single-quoted shell word a literal quote is `'\''`.
    expect(sample).toContain("it'\\''s-model")
    expect(sample).not.toContain(`"model": "vendor/it's-model"`)
  })

  it('escapes quote-bearing model ids in the Python and Node samples', () => {
    const spec = callSpec('chat', 'vendor/quote"model', false)

    expect(callSample('python', spec, CONFIGURED)).toContain(
      'model="vendor/quote\\"model"',
    )
    expect(callSample('node', spec, CONFIGURED)).toContain(
      'model: "vendor/quote\\"model"',
    )
  })

  it('uses python requests and node fetch for rerank because the OpenAI SDK has no rerank', () => {
    const spec = callSpec('rerank', 'cohere/rerank-v3', false)

    const python = callSample('python', spec, CONFIGURED)
    expect(python).toContain('import requests')
    expect(python).toContain('requests.post(')
    expect(python).not.toContain('from openai')

    const node = callSample('node', spec, CONFIGURED)
    expect(node).toContain('fetch(')
    expect(node).not.toContain('openai')
  })

  it('streams chat only when the spec asks for it', () => {
    const streaming = callSample(
      'python',
      callSpec('chat', MODEL_ID, true),
      CONFIGURED,
    )
    expect(streaming).toContain('stream=True')

    const plain = callSample(
      'python',
      callSpec('chat', MODEL_ID, false),
      CONFIGURED,
    )
    expect(plain).not.toContain('stream=True')
  })
})

// 自建部署的 server_address 常常是私网地址：样例里贴出来等于把访客引到
// 一个他们永远调不通的端点，同时把内网拓扑印在公开页上。
describe('sampleTarget reachability', () => {
  it.each([
    'http://127.0.0.1:3000',
    'http://10.211.55.5:8080',
    'http://192.168.1.10:3000',
    'http://169.254.169.254',
    'http://[fd00::1]:3000',
    'http://user:pass@gateway.example.com',
  ])('treats %s as unconfigured', (address) => {
    expect(sampleTarget(address)).toEqual({ baseUrl: '', configured: false })
  })

  it('keeps a public address, path prefix included', () => {
    expect(sampleTarget('https://gateway.example.com/openai/')).toEqual({
      baseUrl: 'https://gateway.example.com/openai',
      configured: true,
    })
  })
})
