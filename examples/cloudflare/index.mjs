// A private URL accepts pin updates; a separate public route serves the pins.
async function matches(a, b) {
  const hash = async (s) => new Uint8Array(await crypto.subtle.digest('SHA-256', new TextEncoder().encode(s)));
  const [x, y] = await Promise.all([hash(a), hash(b)]);
  let diff = 0;
  for (let i = 0; i < x.length; i++) diff |= x[i] ^ y[i];
  return diff === 0;
}
const response = (body, status = 200) => new Response(body, {
  status, headers: { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' },
});
export default {
  async fetch(request, env) {
    const url = new URL(request.url);
    const key = 'certificate-pins';
    if (url.pathname === '/certificates' && request.method === 'GET') {
      const value = await env.PINS.get(key);
      return response(value ?? '{"error":"No certificates published"}', value ? 200 : 503);
    }
    if (!env.WEBHOOK_SECRET || env.WEBHOOK_SECRET.length < 32) return response('{"error":"Webhook not configured"}', 503);
    if (request.method !== 'PUT' || url.search || !(await matches(url.pathname, `/hooks/certificates/${env.WEBHOOK_SECRET}`))) {
      return response('{"error":"Not found"}', 404);
    }
    if (request.headers.get('Content-Type')?.split(';')[0].trim() !== 'application/json') return response('{}', 415);
    const reader = request.body?.getReader();
    if (!reader) return response('{}', 400);
    let text = '', size = 0;
    const decoder = new TextDecoder();
    while (true) {
      const {done, value} = await reader.read();
      if (done) break;
      size += value.byteLength;
      if (size > 8192) { await reader.cancel(); return response('{}', 413); }
      text += decoder.decode(value, {stream: true});
    }
    text += decoder.decode();
    let data;
    try { data = JSON.parse(text); } catch { return response('{}', 400); }
    if (data.version !== 1 || !Array.isArray(data.certificates) || data.certificates.length < 1 || data.certificates.length > 2) return response('{}', 400);
    const certificates = [];
    for (const p of data.certificates) {
      const from = Date.parse(p.notBefore), to = Date.parse(p.notAfter);
      if (p.algorithm !== 'sha-256' || typeof p.value !== 'string' || !/^[A-Za-z0-9+/]{43}=$/.test(p.value) || !Number.isFinite(from) || !Number.isFinite(to) || to <= from || to - from > 14 * 86400000) return response('{}', 400);
      certificates.push({algorithm: p.algorithm, value: p.value, notBefore: p.notBefore, notAfter: p.notAfter});
    }
    // Acknowledge only after storage succeeds. Repeated PUTs replace the same value.
    await env.PINS.put(key, JSON.stringify({version: 1, certificates}));
    return new Response(null, {status: 204, headers: {'Cache-Control': 'no-store'}});
  },
};
