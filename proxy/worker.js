/**
 * whpaper mirror — a deliberately narrow reverse proxy for wallhaven.cc.
 *
 * Why: wallhaven.cc is unreachable/poisoned from mainland China. Cloudflare is
 * reachable (especially over a preferred-IP pool), and every wallhaven host is
 * already behind Cloudflare, so a Worker on your own domain is a ~10-line win:
 *
 *   client ──HTTPS──▶ https://wh.example.com/api/v1/search...   → wallhaven.cc
 *   client ──HTTPS──▶ https://wh.example.com/full/qd/….jpg      → w.wallhaven.cc
 *
 * The JSON body is rewritten so `path` / `thumbs` point back at this mirror,
 * which means the client needs no special-casing beyond a different base URL.
 *
 * Security:
 *   - only the path prefixes in ROUTES are ever fetched (no open proxy);
 *   - optional shared secret via WHPAPER_TOKEN (header x-wh-token or ?t=);
 *   - cookies / authorization are stripped before hitting the origin.
 *
 * Deploy: see proxy/README.md (wrangler, or the curl script that needs no node).
 */

const UA = 'Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36';

const SITE = 'https://wallhaven.cc';
const FULL = 'https://w.wallhaven.cc';
const THUMB = 'https://th.wallhaven.cc';

/** @type {{prefix:string, origin:string, ttl:number, rewrite:boolean}[]} */
const ROUTES = [
  { prefix: '/api/', origin: SITE, ttl: 0, rewrite: true },
  { prefix: '/full/', origin: FULL, ttl: 604800, rewrite: false },
  { prefix: '/original/', origin: FULL, ttl: 604800, rewrite: false },
  { prefix: '/large/', origin: FULL, ttl: 604800, rewrite: false },
  { prefix: '/medium/', origin: FULL, ttl: 604800, rewrite: false },
  { prefix: '/small/', origin: FULL, ttl: 604800, rewrite: false },
  { prefix: '/lg/', origin: THUMB, ttl: 604800, rewrite: false },
  { prefix: '/orig/', origin: THUMB, ttl: 604800, rewrite: false },
  { prefix: '/sm/', origin: THUMB, ttl: 604800, rewrite: false },
  { prefix: '/images/', origin: SITE, ttl: 86400, rewrite: false },
  { prefix: '/w/', origin: SITE, ttl: 3600, rewrite: false },
];

const ORIGIN_HOSTS = ['wallhaven.cc', 'w.wallhaven.cc', 'th.wallhaven.cc', 'whvn.cc'];

export default {
  async fetch(request, env, ctx) {
    const url = new URL(request.url);

    if (request.method === 'OPTIONS') {
      return new Response(null, { status: 204, headers: cors() });
    }
    if (request.method !== 'GET' && request.method !== 'HEAD') {
      return json(405, { error: 'method not allowed' });
    }

    if (url.pathname === '/healthz') {
      return json(200, { ok: true, service: 'whpaper-mirror', routes: ROUTES.map((r) => r.prefix) });
    }

    const token = (env.WHPAPER_TOKEN || '').trim();
    if (token) {
      const got = request.headers.get('x-wh-token') || url.searchParams.get('t') || '';
      if (got !== token) {
        return json(403, { error: 'missing or invalid token' });
      }
    }

    const route = ROUTES.find((r) => url.pathname.startsWith(r.prefix));
    if (!route) {
      return json(404, { error: 'path is not proxied', path: url.pathname });
    }

    // Edge cache for the mirror itself (images only; random API results must not
    // be cached, and ranged requests are passed straight through).
    const cacheable = route.ttl > 0 && !request.headers.get('range');
    const cache = caches.default;
    const key = new Request(url.toString(), { method: 'GET' });
    if (cacheable) {
      const hit = await cache.match(key);
      if (hit) {
        const h = new Headers(hit.headers);
        h.set('x-wh-cache', 'HIT');
        return new Response(hit.body, { status: hit.status, statusText: hit.statusText, headers: h });
      }
    }

    const target = route.origin + url.pathname + url.search;
    const headers = new Headers(request.headers);
    for (const drop of ['cookie', 'authorization', 'x-wh-token', 'host', 'referer',
      'cf-connecting-ip', 'cf-ipcountry', 'cf-ray', 'cf-visitor', 'x-forwarded-for',
      'x-forwarded-host', 'x-forwarded-proto', 'x-real-ip', 'accept-encoding']) {
      headers.delete(drop);
    }
    headers.set('host', new URL(route.origin).host);
    headers.set('user-agent', env.WHPAPER_UA || UA);
    headers.set('accept-encoding', 'identity');
    if (request.headers.get('range')) {
      headers.set('range', request.headers.get('range'));
    }

    let upstream;
    try {
      upstream = await fetch(target, {
        method: request.method,
        headers,
        redirect: 'manual',
        cf: route.ttl > 0 ? { cacheTtl: route.ttl, cacheKey: target } : undefined,
      });
    } catch (e) {
      return json(502, { error: 'upstream fetch failed', detail: String((e && e.message) || e) });
    }

    const respHeaders = new Headers(upstream.headers);
    respHeaders.delete('set-cookie');
    respHeaders.delete('cf-ray');
    respHeaders.delete('server');
    for (const [k, v] of Object.entries(cors())) {
      respHeaders.set(k, v);
    }
    respHeaders.set('x-wh-upstream', new URL(route.origin).host);

    const ctype = (respHeaders.get('content-type') || '').toLowerCase();

    // Redirects: rewrite Location so the client stays on the mirror.
    if (upstream.status >= 300 && upstream.status < 400) {
      const loc = respHeaders.get('location');
      if (loc) {
        respHeaders.set('location', rewriteHosts(loc, url.origin));
      }
      return new Response(upstream.body, {
        status: upstream.status, statusText: upstream.statusText, headers: respHeaders,
      });
    }

    if (ctype.includes('application/json') && route.rewrite) {
      const text = rewriteHosts(await upstream.text(), url.origin);
      respHeaders.delete('content-encoding');
      respHeaders.delete('content-length');
      respHeaders.set('content-length', String(new Blob([text]).size));
      respHeaders.set('cache-control', 'no-store');
      return new Response(text, {
        status: upstream.status, statusText: upstream.statusText, headers: respHeaders,
      });
    }

    const out = new Response(upstream.body, {
      status: upstream.status, statusText: upstream.statusText, headers: respHeaders,
    });
    if (cacheable && upstream.status === 200) {
      // Clone before the body is consumed by the caller.
      const toCache = out.clone();
      toCache.headers.set('cache-control', `public, max-age=${route.ttl}`);
      ctx.waitUntil(cache.put(key, toCache));
    }
    return out;
  },
};

function rewriteHosts(text, origin) {
  // wallhaven's JSON escapes slashes ("https:\/\/w.wallhaven.cc\/full\/..."),
  // so both the escaped and the plain form have to be handled.
  const bare = origin.replace(/^https?:\/\//, '');
  let out = text;
  for (const host of ORIGIN_HOSTS) {
    out = out.split(`https:\\/\\/${host}`).join(`https:\\/\\/${bare}`);
    out = out.split(`http:\\/\\/${host}`).join(`http:\\/\\/${bare}`);
    out = out.split(`https://${host}`).join(origin);
    out = out.split(`http://${host}`).join(origin);
  }
  return out;
}

function cors() {
  return {
    'access-control-allow-origin': '*',
    'access-control-allow-methods': 'GET, HEAD, OPTIONS',
    'access-control-allow-headers': 'Range, X-WH-Token, Accept',
    'access-control-expose-headers': 'Content-Range, Accept-Ranges, X-WH-Cache',
  };
}

function json(status, obj) {
  return new Response(JSON.stringify(obj), {
    status,
    headers: { 'content-type': 'application/json; charset=utf-8', ...cors() },
  });
}
