# Certificate pin webhook for Cloudflare Workers

A minimal receiver backed by Workers KV. It provides:

- `PUT /hooks/certificates/<secret>`: replace the stored certificate pins.
- `GET /certificates`: return public certificate pins, without the webhook secret.

Deploy this Worker separately, or integrate its routes into your existing frontend Worker. A separate deployment requires your frontend server to proxy the public route, or an explicit CORS policy for browser reads.

## Setup

Requires Node.js and a Cloudflare account with Workers and KV access.

```sh
cd examples/cloudflare
npx wrangler kv namespace create bridge-pins
```

Copy the namespace ID into `wrangler.jsonc`, then deploy:

```sh
npx wrangler deploy
```

Use the deployed Worker's HTTPS origin to generate a private webhook URL:

```sh
node create-webhook-url.mjs https://webtransport-bridge-pins.YOUR-SUBDOMAIN.workers.dev
npx wrangler secret put WEBHOOK_SECRET < .webhook-secret
```

The script generates a 256-bit random secret, saves it locally with mode `0600`, and prints the complete webhook URL. Running it again reuses the saved secret. Set that URL as `CERT_WEBHOOK_URL` in the gateway's `.env`, not in source control. Never use the public `/certificates` route as the upload URL.

The secret file is ignored by Git. The URL grants permission to replace pins; keep it out of frontend bundles and shared logs. Your ingress or observability configuration may record URL paths, so restrict or redact those logs. Rotate a leaked secret by replacing both the Worker secret and the gateway URL.

## Delivery contract

The gateway sends JSON with `version: 1` and a `certificates` array containing the active pin and, during rotation, the next pin. Each entry includes `algorithm: "sha-256"`, a base64 `value`, `notBefore`, and `notAfter`.

The receiver validates the payload and responds with `204` only after `KV.put()` completes. Duplicate PUTs replace the same key. A failed write must return an error, never a success or `202`.

KV acknowledgements do not guarantee immediate global visibility. Keep the gateway's propagation delay (one hour by default), avoid adding a long-lived cache to the public route, and refresh pins before establishing a new connection. Clients should refresh and retry when a connection fails.

This example uses one KV key per Worker and assumes one gateway publisher. Use a separate secret and key for each additional gateway. It does not accept private keys or publish access tokens.

## Tests

```sh
node --test index.test.mjs
```
