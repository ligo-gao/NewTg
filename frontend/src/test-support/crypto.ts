/**
 * Ephemeral test keys for the login-encryption tests.
 *
 * Real deployment keys must never appear in the repository or in fixtures, so
 * every test generates a fresh 2048-bit RSA-OAEP pair at runtime. The helper
 * also exposes the SPKI PEM shape the backend serves and the base64 helpers
 * used to assert the `v2.` envelope layout.
 */

export interface TestLoginKey {
  kid: string
  publicKeyPEM: string
  keyPair: CryptoKeyPair
}

export function bytesToBase64(bytes: Uint8Array): string {
  // Keys and envelopes here are far below the argument limit, so spreading is safe.
  return btoa(String.fromCharCode(...bytes))
}

export function base64ToBytes(value: string): Uint8Array<ArrayBuffer> {
  const binary = atob(value)
  const bytes = new Uint8Array(binary.length)
  for (let i = 0; i < binary.length; i += 1) bytes[i] = binary.charCodeAt(i)
  return bytes
}

export function pemToBytes(pem: string): Uint8Array<ArrayBuffer> {
  const body = pem
    .replace(/-----BEGIN PUBLIC KEY-----/, '')
    .replace(/-----END PUBLIC KEY-----/, '')
    .replace(/\s+/g, '')
  return base64ToBytes(body)
}

export function bytesToPem(bytes: Uint8Array): string {
  const base64 = bytesToBase64(bytes)
  const lines = base64.match(/.{1,64}/g) ?? [base64]
  return `-----BEGIN PUBLIC KEY-----\n${lines.join('\n')}\n-----END PUBLIC KEY-----\n`
}

export async function generateLoginKey(kid = 'test-kid-0001'): Promise<TestLoginKey> {
  const keyPair = (await crypto.subtle.generateKey(
    {
      name: 'RSA-OAEP',
      modulusLength: 2048,
      publicExponent: new Uint8Array([0x01, 0x00, 0x01]),
      hash: 'SHA-256',
    },
    true,
    ['encrypt', 'decrypt'],
  )) as CryptoKeyPair

  const spki = await crypto.subtle.exportKey('spki', keyPair.publicKey)
  return {
    kid,
    publicKeyPEM: bytesToPem(new Uint8Array(spki)),
    keyPair,
  }
}
