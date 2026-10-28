/**
 * Public-facing URL derivation shared by both footers.
 *
 * `site.server_address` is what an operator typed into settings — on a
 * self-hosted deployment that is usually the LAN address or `localhost`, which
 * is exactly the address a visitor cannot reach. Rendering it in the footer
 * looks like data but is a dead end, and a URL carrying userinfo would print
 * credentials onto a public page. Both apps must therefore apply the same
 * rule; it lives here so they cannot drift.
 */

/** IPv4 ranges that are loopback, private, link-local, CGNAT or unspecified. */
function isPrivateIPv4(host: string): boolean {
  const parts = host.split('.');
  if (parts.length !== 4) return false;
  const octets = parts.map((part) => (/^\d{1,3}$/.test(part) ? Number(part) : Number.NaN));
  if (octets.some((octet) => Number.isNaN(octet) || octet > 255)) return false;
  const a = octets[0] ?? 0;
  const b = octets[1] ?? 0;
  if (a === 0 || a === 10 || a === 127) return true;
  if (a === 169 && b === 254) return true; // link-local, incl. cloud metadata
  if (a === 172 && b >= 16 && b <= 31) return true;
  if (a === 192 && b === 168) return true;
  if (a === 100 && b >= 64 && b <= 127) return true; // CGNAT
  return false;
}

/** IPv4 dotted form of an IPv4-mapped IPv6 host (`::ffff:7f00:1`). */
function mappedIPv4(host: string): string | null {
  const match = /^::ffff:(.+)$/.exec(host);
  if (!match) return null;
  const tail = match[1] ?? '';
  if (tail.includes('.')) return tail;
  const groups = tail.split(':');
  if (groups.length !== 2) return null;
  const hi = Number.parseInt(groups[0] ?? '', 16);
  const lo = Number.parseInt(groups[1] ?? '', 16);
  if (Number.isNaN(hi) || Number.isNaN(lo)) return null;
  return `${hi >> 8}.${hi & 0xff}.${lo >> 8}.${lo & 0xff}`;
}

function isPrivateIPv6(host: string): boolean {
  const mapped = mappedIPv4(host);
  if (mapped) return isPrivateIPv4(mapped);
  if (host === '::' || host === '::1') return true;
  if (/^f[cd][0-9a-f]{2}:/.test(host)) return true; // fc00::/7 unique-local
  if (/^fe[89ab][0-9a-f]:/.test(host)) return true; // fe80::/10 link-local
  return false;
}

function isUnreachableHost(hostname: string): boolean {
  // WHATWG URL keeps the brackets around IPv6 literals.
  const host = hostname.toLowerCase().replace(/^\[|\]$/g, '');
  if (host === '' || host === 'localhost' || host.endsWith('.localhost')) return true;
  if (host.includes(':')) return isPrivateIPv6(host);
  return isPrivateIPv4(host);
}

/**
 * `serverAddress` → the copyable OpenAI-compatible base URL, or `""` when the
 * address is not something a public visitor could call.
 *
 * Preserves a path prefix (`https://host/openai` → `https://host/openai/v1`);
 * query and fragment are rejected because appending `/v1` after them would
 * produce a different URL than the operator configured.
 */
export function publicApiEndpoint(serverAddress: string | undefined | null): string {
  const raw = (serverAddress ?? '').trim().replace(/\/+$/, '');
  if (!/^https?:\/\//i.test(raw)) return '';

  let url: URL;
  try {
    url = new URL(raw);
  } catch {
    return '';
  }
  if (url.protocol !== 'http:' && url.protocol !== 'https:') return '';
  if (url.username !== '' || url.password !== '') return '';
  if (url.search !== '' || url.hash !== '') return '';
  if (isUnreachableHost(url.hostname)) return '';

  return `${raw}/v1`;
}
