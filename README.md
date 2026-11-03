# webtransport-bridge

[![Tests](https://github.com/longern/webtransport-bridge/actions/workflows/test.yaml/badge.svg)](https://github.com/longern/webtransport-bridge/actions/workflows/test.yaml)
[![Container image](https://github.com/longern/webtransport-bridge/actions/workflows/publish.yaml/badge.svg)](https://github.com/longern/webtransport-bridge/actions/workflows/publish.yaml)

Expose an existing HTTP service over WebTransport. Each bidirectional stream carries one HTTP/1.1 request and response, with concurrent requests sharing a WebTransport session.

Use it in front of a file server, WebDAV server, or HTTP API reachable from the gateway. The backend remains responsible for serving files and implementing application behavior.

> **Experimental:** This project defines its own per-stream mapping of HTTP/1.1 messages onto WebTransport. It is not a standardized HTTP transport. A browser client library is not included, and browser interoperability has not yet been verified.

## Features

- Stream uploads and downloads without buffering entire files.
- Forward standard HTTP headers, including Range requests and cache validators.
- Authenticate each request with a Bearer token.
- Restrict requests to a fixed backend, allowed origins, methods, and path prefixes.
- Configure request deadlines, upload limits, and concurrency limits.
- Deploy using Docker images for `linux/amd64` and `linux/arm64`.

## Quick start

You need Docker Compose, a reachable HTTP backend, and a TLS certificate. The example below generates a short-lived self-signed certificate for certificate pinning.

### 1. Configure the gateway

```sh
git clone https://github.com/longern/webtransport-bridge.git
cd webtransport-bridge
```

Create a `.env` file containing a strong random token:

```dotenv
BRIDGE_TOKEN=replace-with-a-long-random-token
```

Edit the arguments in [`compose.yaml`](compose.yaml):

```yaml
command:
  - -backend=http://192.168.1.10:8080
  - -origins=https://app.example.com
  - -paths=/public
  - -cert=/certs/cert.pem
  - -key=/certs/key.pem
```

| Argument | Example | Purpose |
| --- | --- | --- |
| `-backend` | `http://192.168.1.10:8080` | Fixed HTTP(S) backend origin, without credentials, path, query, or fragment. |
| `-origins` | `https://app.example.com` | Comma-separated exact HTTPS page origins, without trailing slashes. |
| `-paths` | `/public` | Comma-separated allowed path prefixes, matched at path-segment boundaries. |

A request for `/public/photo.jpg` is forwarded to `http://192.168.1.10:8080/public/photo.jpg`. Paths are preserved; `/public` is a URL prefix, not a filesystem mount.

The backend must be reachable **from inside the container**. Use its LAN address or Docker service name; `127.0.0.1` refers to the gateway container itself.

### 2. Generate a certificate

Replace `bridge.example.com` with the public hostname or IP address clients will connect to, without a scheme or port:

```sh
docker compose pull
mkdir -p certs

docker run --rm --user "$(id -u):$(id -g)" \
  -v "$PWD/certs:/certs" \
  --entrypoint certgen \
  ghcr.io/longern/webtransport-bridge:latest \
  -host bridge.example.com -out /certs
```

The generator writes `cert.pem` and `key.pem` and prints the certificate's SHA-256 hash as base64. A browser client using `serverCertificateHashes` must decode that hash into 32 bytes.

Certificates use ECDSA P-256 and expire after **13 days**. Certificate generation refuses to overwrite existing files. Rotation and client pin distribution must be managed separately.

The gateway runs as UID `10001`. On Linux, give that user access to the certificate directory and files:

```sh
sudo chown -R 10001:10001 certs
```

You can also supply an existing certificate trusted by your clients, using the configured certificate and key paths.

### 3. Start the gateway

```sh
docker compose up -d
docker compose logs -f
```

The WebTransport endpoint is:

```text
https://bridge.example.com:4433/webtransport
```

Expose or forward **UDP port 4433**. A UDP tunnel must forward to the gateway's published UDP port. The browser connects directly to this endpoint, independently of where the frontend is hosted.

This endpoint requires a WebTransport client. It cannot be used as a regular HTTPS file URL in an address bar, an `<img>` source, or an unmodified HTTP client.

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
