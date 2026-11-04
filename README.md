# webtransport-bridge

[![Tests](https://github.com/longern/webtransport-bridge/actions/workflows/test.yaml/badge.svg)](https://github.com/longern/webtransport-bridge/actions/workflows/test.yaml)
[![Container image](https://github.com/longern/webtransport-bridge/actions/workflows/publish.yaml/badge.svg)](https://github.com/longern/webtransport-bridge/actions/workflows/publish.yaml)

Expose an existing HTTP service over WebTransport. Each bidirectional stream carries one HTTP/1.1 request and response, with concurrent requests sharing a WebTransport session.

Use it in front of a file server, WebDAV server, or HTTP API reachable from the gateway. The backend remains responsible for serving files and implementing application behavior.

> **Experimental:** This project defines its own per-stream mapping of HTTP/1.1 messages onto WebTransport. It is not a standardized HTTP transport. A browser client library is not included, and browser interoperability has not yet been verified.

## Features

- Stream uploads and downloads without buffering entire files.
- Forward standard HTTP headers, including Range requests and cache validators.
- Generate and rotate short-lived certificates automatically, with private webhook delivery of pins.
- Authenticate each request with a Bearer token.
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

Create a `.env` file containing a strong random token:

```dotenv
BRIDGE_TOKEN=replace-with-a-long-random-token
CERT_WEBHOOK_URL=https://pins.example.com/hooks/certificates/your-random-secret
```

Edit the arguments in [`compose.yaml`](compose.yaml):

```yaml
command:
  - -backend=http://192.168.1.10:8080
  - -origins=https://app.example.com
  - -paths=/public
  - -auto-cert
  - -cert-dir=/certs
```

| Argument | Example | Purpose |
| --- | --- | --- |
| `-backend` | `http://192.168.1.10:8080` | Fixed HTTP(S) backend origin, without credentials, path, query, or fragment. |
| `-origins` | `https://app.example.com` | Comma-separated exact HTTPS page origins, without trailing slashes. |
| `-paths` | `/public` | Comma-separated allowed path prefixes, matched at path-segment boundaries. |

A request for `/public/photo.jpg` is forwarded to `http://192.168.1.10:8080/public/photo.jpg`. Paths are preserved; `/public` is a URL prefix, not a filesystem mount.

The backend must be reachable **from inside the container**. Use its LAN address or Docker service name; `127.0.0.1` refers to the gateway container itself.

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

Expose or forward **UDP port 4433**. A UDP tunnel must forward to the gateway's published UDP port. The browser connects directly to this endpoint, independently of where the frontend is hosted.

This endpoint requires a WebTransport client. It cannot be used as a regular HTTPS file URL in an address bar, an `<img>` source, or an unmodified HTTP client.

## Certificate rotation

Automatic mode uses ECDSA P-256 certificates with a 14-day validity period (the WebTransport certificate-hash limit, measured from NotBefore to NotAfter). The validity start is backdated by one minute for clock skew; the expiry is calculated from that start, so the total period does not exceed 14 days. No public hostname is required when clients authenticate using `serverCertificateHashes`: new certificates use the label `webtransport-bridge.invalid`. To override it, optionally set `BRIDGE_HOST` in Compose or pass `-cert-host`. Omitting the setting preserves certificates already stored in the persistent volume; it does not regenerate their keys or change their pins. When fewer than 48 hours remain, the gateway generates and persists the next certificate and publishes both pins. After the receiver confirms storage, it waits one hour before activating the next certificate for new TLS connections. Existing connections are not restarted.

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

For externally managed certificates, omit `-auto-cert` and supply `-cert` and `-key`. The standalone `certgen` tool remains available; automatic rotation and webhook delivery apply only to automatic mode.

## Container image

```text
ghcr.io/longern/webtransport-bridge:latest
```

Images are built and pushed by [GitHub Actions](https://github.com/longern/webtransport-bridge/actions/workflows/publish.yaml) after static checks and race tests pass. Commit-specific tags use the format `sha-<short-commit>`; version tags are generated from `v*` Git tags.

If pulling returns `denied`, check the [package visibility](https://github.com/users/longern/packages/container/package/webtransport-bridge) or authenticate to GHCR. Publishing a public source repository does not automatically make its container package public.

## Configuration

Run `bridge -help` for the command-line reference.

| Flag | Default | Description |
| --- | --- | --- |
| `-listen` | `:4433` | UDP listen address. |
| `-backend` | Required | Fixed HTTP(S) backend origin. |
| `-origins` | Required | Comma-separated allowed HTTPS origins. |
| `-paths` | `/` | Comma-separated allowed URL path prefixes. |
| `-methods` | `GET,HEAD,POST,PUT` | Comma-separated allowed methods. |
| `-auto-cert` | `false` | Enable certificate generation, rotation, and webhook publication. |
| `-cert-host` | `webtransport-bridge.invalid` for new certificates | Optional certificate hostname or IP; existing certificates are reused when omitted. |
| `-cert-dir` | `/certs` | Persistent directory for automatic certificate state. |
| `-cert-publish-delay` | `1h` | Delay after publication before activating the next certificate; between 1m and 24h. |
| `-cert` | `/certs/cert.pem` | TLS certificate PEM file. |
| `-key` | `/certs/key.pem` | TLS private key PEM file. |
| `-timeout` | `5m` | Total deadline per exchange, including file transfers. |
| `-max-headers` | `32768` | Maximum request or backend response header size in bytes. |
| `-max-body` | `1073741824` | Maximum upload size in bytes; does not limit downloads. |
| `-max-streams` | `32` | Maximum active HTTP exchanges across all sessions. |
| `-max-sessions` | `128` | Maximum active WebTransport sessions. |
| `-public` | `false` | Allow requests without a Bearer token. |

Set `BRIDGE_TOKEN` in the environment unless `-public` is enabled. The token belongs in each tunneled HTTP request, not the WebTransport endpoint URL. It is removed before forwarding, so the backend's `Authorization` header cannot be used for separate credentials through this interface.

For public file access, use a dedicated read-only backend and restrict methods to `GET,HEAD`.

## Wire protocol

Open a session at `/webtransport`, then create a bidirectional stream for each HTTP exchange. Requests use HTTP/1.1 origin-form with CRLF line endings and an empty line after the headers:

```http
GET /public/photo.jpg HTTP/1.1
Host: nas.internal
Authorization: Bearer your-token
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
-methods GET,HEAD,PUT,POST,DELETE,OPTIONS,PROPFIND,MKCOL,COPY,MOVE,LOCK,UNLOCK
```

### Errors and limits

| HTTP status | Meaning |
| --- | --- |
| `400` | Invalid request or request headers exceeding the limit. |
| `401` | Missing or invalid Bearer token. |
| `403` | Disallowed or ambiguous path. |
| `405` | Method not enabled. |
| `413` | Request body exceeds the upload limit. |
| `417` | Unsupported Expect or Upgrade header. |
| `502` | Backend exchange failed or returned a protocol upgrade. |
| `504` | Exchange deadline exceeded, when an error response can still be sent. |

An error after response transmission begins, or after a stream deadline expires, may terminate the stream instead of producing an HTTP error. Excess HTTP exchanges are reset; excess session handshakes receive `503`.

A streamed upload can reach the backend partially before its size limit is exceeded. Backends requiring atomic writes should implement temporary storage and commit semantics. Path restrictions do not protect against backend-specific routing behavior or filesystem symlinks.

Sessions are established before per-request authentication. Origin checks and application limits do not replace connection-level rate limiting for exposed deployments.

## Development

Requires Go 1.26 or later.

```sh
go mod tidy
go run ./cmd/certgen -host localhost -out certs
export BRIDGE_TOKEN='replace-with-a-long-random-token'

go run ./cmd/bridge \
  -backend http://127.0.0.1:8080 \
  -origins https://app.example.com \
  -paths /public \
  -cert certs/cert.pem -key certs/key.pem
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

- [HTTP/1.1 message syntax and framing — RFC 9112](https://www.rfc-editor.org/rfc/rfc9112)
- [webtransport-go](https://github.com/quic-go/webtransport-go)
- [quic-go WebTransport documentation](https://quic-go.net/docs/webtransport/)
