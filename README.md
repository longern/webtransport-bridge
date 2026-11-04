# webtransport-bridge

[![Tests](https://github.com/longern/webtransport-bridge/actions/workflows/test.yaml/badge.svg)](https://github.com/longern/webtransport-bridge/actions/workflows/test.yaml)
[![Container image](https://github.com/longern/webtransport-bridge/actions/workflows/publish.yaml/badge.svg)](https://github.com/longern/webtransport-bridge/actions/workflows/publish.yaml)

Expose an existing HTTP service over WebTransport. Each bidirectional stream carries one HTTP/1.1 request and response, with concurrent requests sharing a WebTransport session.

Use it with HTTP APIs, application servers, file servers, or other HTTP backends reachable from the gateway. The backend remains responsible for authentication and application behavior.

> **Experimental:** This project defines its own per-stream mapping of HTTP/1.1 messages onto WebTransport. It is not a standardized HTTP transport. A browser client library is not included, and browser interoperability has not yet been verified.

## Features

- Stream uploads and downloads without buffering entire files.
- Forward standard HTTP headers, including Range requests and cache validators.
- Generate and rotate short-lived certificates automatically, with private webhook delivery of pins.
- Optionally authenticate access to the gateway using standard proxy authentication while preserving backend credentials.
- Restrict requests to a fixed backend, allowed origins, methods, and path prefixes.
- Configure request deadlines, upload limits, and concurrency limits.
- Deploy using Docker images for `linux/amd64` and `linux/arm64`.

## Quick start

You need Docker Compose, a reachable HTTP backend, and an HTTPS webhook that stores certificate pins. A [Cloudflare Worker + KV receiver](examples/cloudflare/README.md) is included. The default Compose setup generates and rotates certificates automatically.

### 1. Configure the gateway

```sh
git clone https://github.com/longern/webtransport-bridge.git
cd webtransport-bridge
```

Create a `.env` file. Choose gateway authentication or delegate authentication to your backend:

```dotenv
# Gateway authentication (default):
BRIDGE_TOKEN=replace-with-a-long-random-token
# For a backend that already authenticates requests, set BRIDGE_PUBLIC=true
# and omit BRIDGE_TOKEN.
CERT_WEBHOOK_URL=https://pins.example.com/hooks/certificates/your-random-secret
```

Edit the arguments in [`compose.yaml`](compose.yaml):

```yaml
command:
  - --public=${BRIDGE_PUBLIC:-false}
  - --backend=http://backend.example.com:8080
  - --origins=https://app.example.com
  - --paths=/public
  - --auto-cert
  - --cert-dir=/certs
```

| Argument | Example | Purpose |
| --- | --- | --- |
| `--backend` | `http://backend.example.com:8080` | Fixed HTTP(S) backend origin, without credentials, path, query, or fragment. |
| `--origins` | `https://app.example.com` | Comma-separated exact HTTPS page origins, without trailing slashes. |
| `--paths` | `/public` | Comma-separated allowed path prefixes, matched at path-segment boundaries. |

A request for `/public/photo.jpg` is forwarded to `http://backend.example.com:8080/public/photo.jpg`. Paths are preserved; `/public` is a URL prefix, not a filesystem mount.

The backend must be reachable **from inside the container**. Use a reachable address or a service name on a shared Docker network; `127.0.0.1` refers to the gateway container itself.

### 2. Configure the private webhook

Deploy the [example receiver](examples/cloudflare/README.md), generate its private URL, and put the URL in `.env` as `CERT_WEBHOOK_URL`. The URL contains a 256-bit random secret; keep it out of frontend code and source control.

The gateway sends an HTTPS PUT containing certificate hashes and validity dates. A successful response must confirm that the data has been stored; `202 Accepted` is not sufficient. Requests time out after 15 seconds, do not follow redirects, and retry every minute on failure. The gateway does not log the webhook URL or response body.

Automatic mode stores certificates and private keys in the persistent `bridge-certs` Docker volume. It does not read manual `cert.pem` / `key.pem` files. Do not share this volume between gateway instances or delete it during routine upgrades.

### 3. Start the gateway

```sh
docker compose pull
docker compose up -d
docker compose logs -f
```

The WebTransport endpoint is:

```text
https://bridge.example.com:4433/webtransport
```

Clients must be able to reach **UDP port 4433** at the advertised endpoint. Any port forwarding must preserve UDP traffic. Clients connect directly to the gateway, independently of where the frontend is hosted.

This endpoint requires a WebTransport client. It cannot be used as a regular HTTPS file URL in an address bar, an `<img>` source, or an unmodified HTTP client.

## Certificate rotation

Automatic mode uses ECDSA P-256 certificates with a 14-day validity period (the WebTransport certificate-hash limit, measured from NotBefore to NotAfter). The validity start is backdated by one minute for clock skew; the expiry is calculated from that start, so the total period does not exceed 14 days. No public hostname is required when clients authenticate using `serverCertificateHashes`: new certificates use the label `webtransport-bridge.invalid`. To override it, optionally set `BRIDGE_HOST` in Compose or pass `--cert-host`. Omitting the setting preserves certificates already stored in the persistent volume; it does not regenerate their keys or change their pins. When fewer than 48 hours remain, the gateway generates and persists the next certificate and publishes both pins. After the receiver confirms storage, it waits one hour before activating the next certificate for new TLS connections. Existing connections are not restarted.

Failed publication prevents activation and is retried every minute. Restarting preserves both certificates, republishes their pins, and restarts the propagation delay. A long outage can still let the active certificate expire; monitor maintenance errors. The delay accommodates eventual consistency but does not guarantee global propagation.

The webhook payload is:

```json
{
  "version": 1,
  "certificates": [
    {
      "algorithm": "sha-256",
      "value": "<base64-encoded SHA-256 of the DER certificate>",
      "notBefore": "<RFC3339 timestamp>",
      "notAfter": "<RFC3339 timestamp>"
    }
  ]
}
```

The array contains up to two entries: active and next. Private keys are never uploaded. Browser clients decode each `value` to 32 bytes for `serverCertificateHashes` and should refresh the configuration before connecting, then refresh and retry after connection failures.

For externally managed certificates, omit `--auto-cert` and supply `--cert` and `--key`. The standalone `certgen` tool remains available; automatic rotation and webhook delivery apply only to automatic mode.

## Container image

```text
ghcr.io/longern/webtransport-bridge:latest
```

Images are built and pushed by [GitHub Actions](https://github.com/longern/webtransport-bridge/actions/workflows/publish.yaml) after static checks and race tests pass. Commit-specific tags use the format `sha-<short-commit>`; version tags are generated from `v*` Git tags.

If pulling returns `denied`, check the [package visibility](https://github.com/users/longern/packages/container/package/webtransport-bridge) or authenticate to GHCR. Publishing a public source repository does not automatically make its container package public.

## Configuration

Run `bridge --help` for the command-line reference.

| Flag | Default | Description |
| --- | --- | --- |
| `--listen` | `:4433` | UDP listen address. |
| `--backend` | Required | Fixed HTTP(S) backend origin. |
| `--origins` | Required | Comma-separated allowed HTTPS origins. |
| `--paths` | `/` | Comma-separated allowed URL path prefixes. |
| `--methods` | `GET,HEAD,POST,PUT` | Comma-separated allowed methods. |
| `--auto-cert` | `false` | Enable certificate generation, rotation, and webhook publication. |
| `--cert-host` | `webtransport-bridge.invalid` for new certificates | Optional certificate hostname or IP; existing certificates are reused when omitted. |
| `--cert-dir` | `/certs` | Persistent directory for automatic certificate state. |
| `--cert-publish-delay` | `1h` | Delay after publication before activating the next certificate; between 1m and 24h. |
| `--cert` | `/certs/cert.pem` | TLS certificate PEM file. |
| `--key` | `/certs/key.pem` | TLS private key PEM file. |
| `--timeout` | `0` | Optional total exchange deadline; `0` disables it. Explicit values from older configurations still apply. |
| `--idle-timeout` | `1m` | Maximum time without successful request or response byte progress on a stream. |
| `--header-timeout` | `15s` | Absolute deadline for receiving the complete client request headers. |
| `--response-header-timeout` | `30s` | Backend response header deadline after the request upload completes. |
| `--keepalive` | `15s` | QUIC keepalive interval; `0` disables it. |
| `--connection-idle-timeout` | `1m` | QUIC connection idle timeout, negotiated with the peer. |
| `--max-headers` | `32768` | Maximum request or backend response header size in bytes. |
| `--max-body` | `1073741824` | Maximum upload size in bytes; does not limit downloads. |
| `--max-streams` | `32` | Maximum active HTTP exchanges across all sessions. |
| `--max-sessions` | `128` | Maximum active WebTransport sessions. |
| `--public` | `false` | Disable gateway token checks and delegate authentication to the backend; any BRIDGE_TOKEN value is ignored. |

### Authentication

`Authorization` and `Cookie` belong to the backend and are forwarded in both modes. Backend `401` responses, `WWW-Authenticate` challenges, and `Set-Cookie` headers are passed back to the client. A WebTransport client must supply backend credentials and handle cookies explicitly: the browser does not apply ordinary Fetch cookie-jar behavior to the inner HTTP messages. The gateway does not translate or manage the backend login flow.

| Mode | Configuration | Client credentials |
| --- | --- | --- |
| Backend authentication only | `BRIDGE_PUBLIC=true` in Compose, or `--public` on the command line | Backend `Authorization` and/or `Cookie`; no gateway token required. |
| Gateway authentication, optionally combined with backend authentication | Set `BRIDGE_TOKEN` and leave `BRIDGE_PUBLIC=false` | `Proxy-Authorization: Bearer <BRIDGE_TOKEN>` plus any backend credentials. |

`--public` explicitly disables gateway authentication even if a stale `BRIDGE_TOKEN` remains in the environment. Without `--public`, an unset or empty token prevents startup. Disabling gateway authentication does not make a protected backend public; the backend continues to enforce its own rules.

The gateway consumes `Proxy-Authorization` and never forwards it, including in public mode. Missing, invalid, or duplicate proxy credentials in protected mode produce `407 Proxy Authentication Required` with `Proxy-Authenticate: Bearer realm="webtransport-bridge"`. A backend authentication failure remains a backend `401`, so clients can distinguish the two layers.

**Migration:** Older versions used `Authorization: Bearer <BRIDGE_TOKEN>` and removed the backend credential. That behavior is no longer supported. Move the gateway token to `Proxy-Authorization`, or enable `BRIDGE_PUBLIC=true` to use only backend authentication. These headers are carried inside the WebTransport stream, not on its browser handshake. Browser Fetch APIs restrict `Proxy-*` headers; a custom WebTransport client should inject proxy credentials while serializing the inner HTTP request, rather than relying on a browser `Request` to preserve them. Keep the gateway token separate from the private certificate webhook URL.

For public file access, use a dedicated read-only backend and restrict methods to `GET,HEAD`.

## Large files and streaming responses

Uploads and downloads stream through the gateway without buffering whole files. There is no default total exchange deadline: an active transfer can run longer than five minutes. Downloads have no size cap; uploads retain the configurable 1 GiB default limit.

An exchange is cancelled if no request or response bytes make progress for one minute. This covers stalled uploads, backend responses, and clients that stop consuming a response. Client cancellation also cancels the backend request and interrupts blocked stream I/O. A separate 15-second absolute request-header deadline prevents a trickle of header bytes from extending setup indefinitely. Backend response headers have their own 30-second timeout after upload; the idle timeout also applies while waiting.

Responses are serialized directly to the stream without an additional buffered writer. SSE headers and events can reach clients before the backend closes its response. The backend must flush its events and send an event or heartbeat before the exchange idle timeout, or you must increase that timeout. QUIC keepalive packets preserve the connection but do not count as HTTP exchange activity. Peer timeouts and network failures can still close connections; clients must handle reconnection.

For example, append these arguments to Compose's `command` for uploads up to 100 GiB and up to five minutes between application bytes:

```yaml
  - --idle-timeout=5m
  - --max-body=107374182400
```

If an existing deployment explicitly sets `--timeout=5m`, remove it or set `--timeout=0` to allow longer transfers. Backend reverse proxies and servers may impose additional limits. WebSocket and HTTP Upgrade forwarding remain unsupported.

## Wire protocol

Open a session at `/webtransport`, then create a bidirectional stream for each HTTP exchange. Requests use HTTP/1.1 origin-form with CRLF line endings and an empty line after the headers:

```http
GET /public/photo.jpg HTTP/1.1
Host: backend.example.com
Proxy-Authorization: Bearer your-gateway-token
Authorization: Bearer your-backend-token
Range: bytes=0-65535

```

The gateway parses requests with Go's `http.ReadRequest`, forwards them using `http.Transport`, and serializes responses with `http.Response.Write`.

- Each stream carries one request and one final response. There is no pipelining.
- Request bodies use `Content-Length` or chunked transfer encoding. Requests without either have no body. Clients should close the stream's send direction after sending the request.
- Clients must parse response framing, including Content-Length, chunked encoding, and stream closure. HEAD responses have no body.
- Range, ETag, conditional requests, and cache policy are implemented by the backend.
- Hop-by-hop headers are removed. Responses are not automatically decompressed, and redirects are returned without being followed.
- Cookies, redirect locations, links, paths, and WebDAV `Destination` headers are not rewritten.
- Response trailers, forwarded interim 1xx responses, `Expect: 100-continue`, protocol upgrades, and application datagrams are not supported.

Additional WebDAV methods can be enabled explicitly, for example:

```text
--methods GET,HEAD,PUT,POST,DELETE,OPTIONS,PROPFIND,MKCOL,COPY,MOVE,LOCK,UNLOCK
```

### Errors and limits

| HTTP status | Meaning |
| --- | --- |
| `400` | Invalid request or request headers exceeding the limit. |
| `407` | Missing, invalid, or duplicate gateway proxy credentials. Backend `401` responses are passed through. |
| `403` | Disallowed or ambiguous path. |
| `405` | Method not enabled; the `Allow` header lists enabled methods. |
| `413` | Request body exceeds the upload limit. |
| `417` | Unsupported Expect or Upgrade header. |
| `502` | Backend exchange failed or returned a protocol upgrade. |
| `504` | Backend timed out before a response began, when an error response can still be sent. |

Idle, request-header, total-deadline, and client-cancellation events cancel the backend request and terminate the stream. An error after response transmission begins also terminates the stream instead of appending an HTTP error. Excess HTTP exchanges are reset; excess session handshakes receive `503`.

A streamed upload can reach the backend partially before its size limit is exceeded. Backends requiring atomic writes should implement temporary storage and commit semantics. Path restrictions do not protect against backend-specific routing behavior or filesystem symlinks.

Sessions are established before per-request authentication. Origin checks and application limits do not replace connection-level rate limiting for exposed deployments.

## Development

Requires Go 1.26 or later.

```sh
go mod tidy
go run ./cmd/certgen --host localhost --out certs
export BRIDGE_TOKEN='replace-with-a-long-random-token'

go run ./cmd/bridge \
  --backend http://127.0.0.1:8080 \
  --origins https://app.example.com \
  --paths /public \
  --cert certs/cert.pem --key certs/key.pem
```

Run checks:

```sh
go vet ./...
go test -race ./...
go build ./cmd/bridge ./cmd/certgen
```

Build a local container image:

```sh
docker build -t webtransport-bridge:local .
```

## References

- [HTTP semantics and proxy authentication — RFC 9110](https://www.rfc-editor.org/rfc/rfc9110)

- [HTTP/1.1 message syntax and framing — RFC 9112](https://www.rfc-editor.org/rfc/rfc9112)
- [webtransport-go](https://github.com/quic-go/webtransport-go)
- [quic-go WebTransport documentation](https://quic-go.net/docs/webtransport/)
