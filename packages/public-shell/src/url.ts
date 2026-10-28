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

/** Expand an IPv6 literal (brackets already stripped) into eight hextets. */
function expandIPv6(host: string): number[] | null {
  const halves = host.split('::');
  if (halves.length > 2) return null;
  const parseGroups = (part: string): number[] | null => {
    if (part === '') return [];
    const groups: number[] = [];
    for (const group of part.split(':')) {
      if (!/^[0-9a-f]{1,4}$/.test(group)) return null;
      groups.push(Number.parseInt(group, 16));
    }
    return groups;
  };
  const head = parseGroups(halves[0] ?? '');
  const tail = halves.length === 2 ? parseGroups(halves[1] ?? '') : [];
  if (head === null || tail === null) return null;
  if (halves.length === 1) return head.length === 8 ? head : null;
  const missing = 8 - head.length - tail.length;
  if (missing < 0) return null;
  return [...head, ...new Array<number>(missing).fill(0), ...tail];
}

/** Dotted IPv4 for the last two hextets of an IPv6 address. */
function dottedFromHextets(hi: number, lo: number): string {
  return `${hi >> 8}.${hi & 0xff}.${lo >> 8}.${lo & 0xff}`;
}

function isPrivateIPv6(host: string): boolean {
  const hextets = expandIPv6(host);
  if (hextets === null) return false;
  if (hextets.every((hextet) => hextet === 0)) return true; // ::
  if (hextets.slice(0, 7).every((hextet) => hextet === 0) && hextets[7] === 1) {
    return true; // ::1
  }
  if ((hextets[0]! & 0xfe00) === 0xfc00) return true; // fc00::/7 unique-local
  if ((hextets[0]! & 0xffc0) === 0xfe80) return true; // fe80::/10 link-local
  // IPv4-mapped (::ffff:a.b.c.d) and IPv4-compatible (::a.b.c.d) forms: the
  // embedded IPv4 sits in the last two hextets.
  if (hextets.slice(0, 5).every((hextet) => hextet === 0) && hextets[5] === 0xffff) {
    return isPrivateIPv4(dottedFromHextets(hextets[6]!, hextets[7]!));
  }
  if (hextets.slice(0, 6).every((hextet) => hextet === 0)) {
    return isPrivateIPv4(dottedFromHextets(hextets[6]!, hextets[7]!));
  }
  // NAT64 well-known prefix 64:ff9b::/96 and 6to4 2002::/16 embed an IPv4.
  if (hextets[0] === 0x0064 && hextets[1] === 0xff9b &&
    hextets.slice(2, 6).every((hextet) => hextet === 0)) {
    return isPrivateIPv4(dottedFromHextets(hextets[6]!, hextets[7]!));
  }
  if (hextets[0] === 0x2002) {
    return isPrivateIPv4(dottedFromHextets(hextets[1]!, hextets[2]!));
  }
  return false;
}

function isUnreachableHost(hostname: string): boolean {
  // WHATWG URL keeps the brackets around IPv6 literals; a trailing dot is the
  // same name to a resolver (`localhost.` still resolves to loopback).
  const host = hostname.toLowerCase().replace(/^\[|\]$/g, '').replace(/\.+$/, '');
  if (host === '' || host === 'localhost' || host.endsWith('.localhost')) return true;
  if (host.includes(':')) return isPrivateIPv6(host);
  return isPrivateIPv4(host);
}

/**
 * `serverAddress` → the operator's base URL without a trailing slash, or `""`
 * when the address is not something a public visitor could call.
 *
 * Preserves a path prefix (`https://host/openai`); query and fragment are
 * rejected because appending `/v1` after them would produce a different URL
 * than the operator configured.
 */
export function publicBaseUrl(serverAddress: string | undefined | null): string {
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

  return raw;
}

/**
 * `serverAddress` → the copyable OpenAI-compatible base URL (`"/v1"` appended),
 * or `""` when the address is not publicly reachable.
 */
export function publicApiEndpoint(serverAddress: string | undefined | null): string {
  const base = publicBaseUrl(serverAddress);
  return base === '' ? '' : `${base}/v1`;
}
