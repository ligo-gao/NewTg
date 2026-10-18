import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import {
  afterAll,
  afterEach,
  beforeAll,
  beforeEach,
  describe,
  expect,
  it,
} from 'vitest'

import { base64ToBytes, generateLoginKey, pemToBytes } from '@/test-support/crypto'

import {
  buildLoginCredentials,
  clearLoginEncryptionKeyCache,
  encryptPasswordV2,
  fetchLoginEncryptionKey,
} from './password-encryption'

const ORIGIN = 'http://localhost:3000'
const KEY_URL = `${ORIGIN}/api/auth/login/encryption-key`

const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
beforeEach(() => clearLoginEncryptionKeyCache())
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

/**
 * Decrypts a v2 envelope with the raw WebCrypto primitives the backend uses.
 * If this round-trip succeeds, the frontend produced exactly the format
 * `EncryptPasswordV2`/`DecryptPasswordV2` expect (standard base64 with padding,
 * OAEP label `password-v2`, AAD `password-v2:<kid>`, GCM tag appended).
 */
async function decryptEnvelope(
  envelope: string,
  kid: string,
  keyPair: CryptoKeyPair,
): Promise<string> {
  const parts = envelope.split('.')
  expect(parts).toHaveLength(4)
  expect(parts[0]).toBe('v2')

  const wrappedKey = base64ToBytes(parts[1] ?? '')
  const nonce = base64ToBytes(parts[2] ?? '')
  const ciphertext = base64ToBytes(parts[3] ?? '')

  const rawDataKey = await crypto.subtle.decrypt(
    { name: 'RSA-OAEP', label: new TextEncoder().encode('password-v2') },
    keyPair.privateKey,
    wrappedKey,
  )
  const dataKey = await crypto.subtle.importKey('raw', rawDataKey, 'AES-GCM', false, [
    'decrypt',
  ])
  const plaintext = await crypto.subtle.decrypt(
    {
      name: 'AES-GCM',
      iv: nonce,
      additionalData: new TextEncoder().encode(`password-v2:${kid}`),
    },
    dataKey,
    ciphertext,
  )
  return new TextDecoder().decode(plaintext)
}

describe('encryptPasswordV2', () => {
  it('produces a v2 envelope that decrypts to the original password', async () => {
    const key = await generateLoginKey('kid-alpha')
    const password = 'correct horse battery staple'

    const envelope = await encryptPasswordV2(password, key.publicKeyPEM, key.kid)

    expect(envelope.startsWith('v2.')).toBe(true)
    await expect(decryptEnvelope(envelope, key.kid, key.keyPair)).resolves.toBe(password)
  })

  it('handles long multi-byte passwords that would not fit bare RSA-OAEP', async () => {
    const key = await generateLoginKey('kid-beta')
    const password = 'pässwörd-🔐-'.repeat(30)

    const envelope = await encryptPasswordV2(password, key.publicKeyPEM, key.kid)

    await expect(decryptEnvelope(envelope, key.kid, key.keyPair)).resolves.toBe(password)
  })
})

describe('fetchLoginEncryptionKey', () => {
  it('returns the server key and caches it for five minutes', async () => {
    const key = await generateLoginKey('kid-cache')
    let calls = 0
    server.use(
      http.get(KEY_URL, () => {
        calls += 1
        return HttpResponse.json({ kid: key.kid, public_key: key.publicKeyPEM })
      }),
    )

    let now = 0
    const options = { now: () => now }

    const first = await fetchLoginEncryptionKey(options)
    now = 4 * 60 * 1000
    const second = await fetchLoginEncryptionKey(options)
    now = 6 * 60 * 1000
    const third = await fetchLoginEncryptionKey(options)

    expect(first).toEqual({ enabled: true, kid: key.kid, publicKey: key.publicKeyPEM })
    expect(second).toBe(first)
    expect(third).toEqual(first)
    expect(calls).toBe(2)
  })

  it('reports the disabled state only when enabled === false', async () => {
    server.use(http.get(KEY_URL, () => HttpResponse.json({ enabled: false })))

    await expect(fetchLoginEncryptionKey()).resolves.toEqual({ enabled: false })
  })

  it('does not cache the disabled state, so enabling applies on the next probe', async () => {
    const key = await generateLoginKey('kid-enabled-later')
    let calls = 0
    server.use(
      http.get(KEY_URL, () => {
        calls += 1
        return calls === 1
          ? HttpResponse.json({ enabled: false })
          : HttpResponse.json({ kid: key.kid, public_key: key.publicKeyPEM })
      }),
    )

    await expect(fetchLoginEncryptionKey()).resolves.toEqual({ enabled: false })
    await expect(fetchLoginEncryptionKey()).resolves.toEqual({
      enabled: true,
      kid: key.kid,
      publicKey: key.publicKeyPEM,
    })
    expect(calls).toBe(2)
  })

  it('rejects a response that lacks key material instead of downgrading', async () => {
    server.use(http.get(KEY_URL, () => HttpResponse.json({ enabled: true })))

    await expect(fetchLoginEncryptionKey()).rejects.toThrow()
  })

  it('rejects when the key endpoint fails (never falls back to plaintext)', async () => {
    server.use(
      http.get(KEY_URL, () =>
        HttpResponse.json({ title: 'unavailable' }, { status: 503 }),
      ),
    )

    await expect(fetchLoginEncryptionKey()).rejects.toThrow()
  })
})

describe('buildLoginCredentials', () => {
  it('submits plaintext only when encryption is explicitly disabled', async () => {
    server.use(http.get(KEY_URL, () => HttpResponse.json({ enabled: false })))

    await expect(buildLoginCredentials('alice', 'secret-pass')).resolves.toEqual({
      identifier: 'alice',
      password: 'secret-pass',
    })
  })

  it('submits the v2 envelope plus kid when encryption is enabled', async () => {
    const key = await generateLoginKey('kid-login')
    server.use(
      http.get(KEY_URL, () =>
        HttpResponse.json({ kid: key.kid, public_key: key.publicKeyPEM }),
      ),
    )

    const credentials = await buildLoginCredentials('alice', 'secret-pass')

    expect(credentials.identifier).toBe('alice')
    expect('password' in credentials).toBe(false)
    if (!('password_encrypted' in credentials)) {
      throw new Error('expected the encrypted credential shape')
    }
    expect(credentials.encryption_key_id).toBe(key.kid)
    expect(credentials.password_encrypted.startsWith('v2.')).toBe(true)
    await expect(
      decryptEnvelope(credentials.password_encrypted, key.kid, key.keyPair),
    ).resolves.toBe('secret-pass')
  })

  it('does not silently downgrade when key retrieval fails', async () => {
    server.use(
      http.get(KEY_URL, () =>
        HttpResponse.json({ title: 'unavailable' }, { status: 503 }),
      ),
    )

    await expect(buildLoginCredentials('alice', 'secret-pass')).rejects.toThrow()
  })
})

describe('public key parsing', () => {
  it('accepts the SPKI PEM produced by the backend', async () => {
    const key = await generateLoginKey('kid-pem')
    // Guard the test-support helper itself: the PEM body must decode.
    expect(pemToBytes(key.publicKeyPEM).byteLength).toBeGreaterThan(250)
  })
})
