# webtransport-bridge

一个薄的 NAS WebTransport → HTTP 网关。每条 WebTransport 双向流承载一个标准 HTTP/1.1 请求及其响应；同一会话通过多条流并发。仅实现服务端，暂不包含浏览器 `tunnelFetch()`。

## Docker 部署

镜像地址：`ghcr.io/longern/webtransport-bridge:latest`，支持 linux/amd64 和 linux/arm64，由 GitHub Actions 测试通过后自动构建推送。

需要 Docker Compose。下载本仓库的 `compose.yaml`，然后修改：

- `-backend`：Docker 内可达的固定 HTTP(S) 后端 origin，例如 NAS 上的 Caddy、WebDAV 或 API。不能带路径。
- `-origins`：允许发起连接的网页来源，完整匹配，可逗号分隔，不带末尾 `/`。
- `-paths`：允许访问的路径前缀，按路径段匹配。例如 `/public` 允许 `/public/a`，不允许 `/publicity`。

```sh
docker compose pull
mkdir -p certs
# 宿主机运行生成器，避免容器 UID 对挂载目录的权限问题。
docker run --rm --user "$(id -u):$(id -g)" \
  -v "$PWD/certs:/certs" --entrypoint certgen \
  ghcr.io/longern/webtransport-bridge:latest -host your-tunnel-host.example -out /certs
```

生成器输出证书 SHA-256 指纹的 base64 表示，后续浏览器客户端需要将其解码为 32 字节 `serverCertificateHashes`。证书使用 ECDSA P-256，有效期 13 天。证书与私钥文件默认权限 0600；需让容器 UID 10001 能读取它们，例如：

```sh
sudo chown -R 10001:10001 certs
```

创建 `.env`，写入 `BRIDGE_TOKEN=<足够长的随机令牌>`，然后启动：

```sh
docker compose up -d
docker compose logs -f
```

容器默认启动命令就是 `bridge`，参数通过 Compose `command` 配置。暴露的是 **UDP 4433**，不是 TCP。SakuraFrp 等隧道应使用 UDP 转发到容器映射的 UDP 端口。网页由 Cloudflare Workers 提供时，WebTransport 连接仍由浏览器直接连 NAS 隧道；Workers 不参与数据转发。

可用受浏览器信任的正式证书替换自签证书。自签证书轮换时必须同步更新前端可信指纹；本项目不包含自动轮换或 Cloudflare 指纹发布。私钥始终保留在 NAS。使用自签 pin 的浏览器支持和 WebTransport draft 兼容性需要在目标浏览器实测。

## 本地运行

使用 Go 1.26：

```sh
go mod tidy
go run ./cmd/certgen -host localhost -out certs
export BRIDGE_TOKEN='replace-with-a-random-token'
go run ./cmd/bridge -backend http://127.0.0.1:8080 \
  -origins https://your-site.workers.dev \
  -paths /public -cert certs/cert.pem -key certs/key.pem
```

```sh
go test -race ./...
go vet ./...
go run ./cmd/bridge -help
```

`webtransport-go` 固定为 v0.13.0。源码构建前执行 `go mod tidy` 生成完整依赖清单和 `go.sum`。镜像发布前运行完整静态检查和 Go 测试；真实浏览器连通性尚未验证。

如需自己构建镜像，可运行 `docker build -t webtransport-bridge:local .`。首次发布的 GHCR 包默认可能为私有，若匿名拉取被拒绝，仓库所有者需到 GitHub Packages 的包设置将可见性改为 Public，或在 NAS 配置仓库登录。

## 协议约束

连接地址：`https://<隧道主机>:<UDP端口>/webtransport`。Origin 必须匹配白名单。浏览器握手不能任意设置 Authorization，所以认证放在**每条内部 HTTP 请求**的 `Authorization: Bearer ...`，不放 URL 查询参数。握手地址不接受查询参数。网关令牌在转发前移除；需要后端独立用户认证的场景，应另行设计凭据传递，不要复用此头。

请求必须使用 HTTP/1.1 origin-form、CRLF 换行，并以空行结束头部：

```http
GET /public/photos/cat.jpg HTTP/1.1
Host: nas.internal
Authorization: Bearer your-token
Range: bytes=0-65535

```

- 一条流只处理一个请求和一个最终响应。无 pipelining、WebSocket、CONNECT 或 datagram 业务协议。
- 请求体必须使用正确的 `Content-Length` 或 HTTP chunked 编码；发送完后客户端应关闭流的发送方向。无长度的请求按 HTTP 标准视为无请求体。
- 响应由 Go `http.Response.Write` 编码。支持 Content-Length、chunked 和连接关闭界定；客户端必须实现相应消息解析，不得假定只有 Content-Length。
- Range、ETag、If-None-Match、Content-Type、Cache-Control 等由后端处理并返回，网关不模拟文件服务。
- 支持流式上传和下载，不将完整文件加载到内存。HEAD 不发送响应体。
- 不转发逐跳头、不自动解压、不跟随后端重定向。客户端收到的 Location 保持后端原值，需自行处理；不做 cookie、链接或路径重写。
- 第一版不传递响应 trailers，不支持 `Expect: 100-continue`，不向客户端输出中间 1xx 响应。
- 默认方法为 GET、HEAD、POST、PUT。WebDAV 可配置 `-methods GET,HEAD,PUT,POST,DELETE,OPTIONS,PROPFIND,MKCOL,COPY,MOVE,LOCK,UNLOCK`。Destination 等 WebDAV 头不重写，需客户端使用后端可接受的值。

## 资源和权限

| 参数 | 默认值 | 含义 |
| --- | --- | --- |
| `-listen` | `:4433` | UDP 监听地址 |
| `-timeout` | `5m` | 每次交换总时限，包含大文件传输 |
| `-max-headers` | `32768` | 内部请求与后端响应头上限 |
| `-max-body` | `1073741824` | 最大上传字节数，下载不受此限制 |
| `-max-streams` | `32` | 全局同时处理的 HTTP 交换数；超限 reset 流 |
| `-max-sessions` | `128` | 活跃会话上限；超限握手返回 503 |
| `-public` | `false` | 显式禁用令牌要求，适合公开只读内容 |

解析失败返回 400，鉴权失败返回 401，路径不允许返回 403，方法不允许返回 405，请求体过大返回 413，不支持 Expect/Upgrade 返回 417，后端失败返回 502，交换超时可返回 504。如果已经开始输出响应或流截止时间已到，则 reset/关闭流，无法再追加 HTTP 错误。

未知长度的上传超过限制时，后端可能已经接收到部分内容；上传限制不是原子写入保证，需后端实现临时文件与提交机制。路径白名单限制的是规范化 URL 路径，不保护后端文件系统的软链接或自有路由；公开访问建议后端另设只读根目录，同时将 `-methods` 限定为 `GET,HEAD`。

用户未认证前仍可建立会话；来源校验不能代替认证或抗 DoS。会话和交换上限用于约束应用负载，公网大规模部署还应在入口做连接限流。

## 实现依据

- [webtransport-go server API](https://pkg.go.dev/github.com/quic-go/webtransport-go@v0.13.0)
- [quic-go WebTransport server](https://quic-go.net/docs/webtransport/server/)
- [RFC 9112](https://www.rfc-editor.org/rfc/rfc9112)

这里复用标准 HTTP/1.1 消息格式；HTTP/1.1 over WebTransport 的每流映射是本项目约定，不宣称为现成标准协议。
