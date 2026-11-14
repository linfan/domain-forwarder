# Domain Forwarder

[English](README_EN.md)

使用 Go 编写的高性能 HTTP/HTTPS 正向代理服务器。

> 构建、配置参考、性能调优、部署与运维内容请见 [DEPLOY.md](DEPLOY.md)。

## 功能特性

- **HTTP 与 HTTPS**：支持明文 HTTP 转发，并通过 CONNECT 方法建立 HTTPS 隧道
- **HTTPS 代理监听**：代理端口可启用 TLS 并使用 Let's Encrypt 的
  `fullchain.pem`，Chrome 等浏览器可直接以 `https://` 代理方式使用
- **HTTP/2 CONNECT**：同时支持 HTTP/1.1 与 HTTP/2 的 CONNECT 隧道，Chrome
  会在单条 TLS 连接上多路复用所有隧道
- **高并发**：连接池容量可配置
- **缓冲区复用**：基于 `sync.Pool` 实现省内存的数据搬运
- **逐跳头处理**：按 RFC 7230 剥离逐跳头（hop-by-hop header），包括 `Connection`
  头中列出的字段
- **X-Forwarded-For**：可选记录客户端原始地址（`add_proxy_headers`）
- **请求编辑**：转发前为请求追加自定义 Header（`edit`），同名 Header 会被覆盖
- **流量镜像**：可选把普通 HTTP 请求异步复制一份投递到另一个 HTTP 服务（`mirror`），
  镜像失败静默处理，不影响原请求
- **SSE 友好转发**：流式响应逐块 flush，并以不压缩的方式透传
- **超时控制**：上游连接/响应头/响应体空闲/响应体总时长，以及服务端读请求头/空闲
  超时均可配置
- **源 IP 黑名单**：可拒绝单个 IP 或 CIDR 网段，返回 403
- **代理认证**：可要求客户端通过 HTTP Basic 认证（单用户，密码支持明文或 bcrypt
  哈希），未认证请求返回 407
- **优雅退出**：收到 SIGINT/SIGTERM 后在 `server.shutdown_timeout` 内处理完在途请求
- **可选文件日志**：通过 `log` 段按大小滚动
- **YAML 配置**：所有配置项均可省略，不提供配置文件时使用内置默认值

## 快速开始

```bash
# 构建全部二进制文件
make build

# 从纳入版本管理的模板生成本地配置（config.yaml 已被 gitignore）
cp config.example.yaml config.yaml

# 使用本地配置启动代理服务（监听 :8080）
make run

# 或者指定配置文件 / 自定义监听地址
./build/domain-forwarder -c config.yaml
./build/domain-forwarder -addr :3128
```

### 使用代理

```bash
# 通过代理发起 HTTP 请求
curl -x http://localhost:8080 http://example.com

# 通过代理发起 HTTPS 请求
curl -x http://localhost:8080 https://example.com

# 若 tls.enabled 已开启，代理本身也要用 https:// 访问
curl -x https://proxy.example.com:8080 https://example.com

# 代理启用认证时（-U，或把凭据写进代理 URL）
curl -U user:pass -x http://localhost:8080 https://example.com
curl -x http://user:pass@localhost:8080 https://example.com

# 设置为环境变量
export http_proxy=http://localhost:8080
export https_proxy=http://localhost:8080
```

## 命令行选项

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `-c, --config` | 无 | YAML 配置文件路径；省略时使用内置默认值 |
| `-addr` | `:8080` | 代理监听地址（优先级高于配置文件） |
| `--version` | - | 打印版本号与构建时间后退出 |

## 配置

`config.example.yaml` 是带完整注释的配置参考：其中每一项都是内置默认值，因此
省略某个键就沿用默认值。优先级为：命令行参数 > 配置文件 > 内置默认值。

配置分为以下几部分：

- 顶层：`addr`、`debug`、`add_proxy_headers`、`source_ip_blacklist`、
  `auth`（代理客户端的 HTTP Basic 认证，单用户，密码可为明文或 bcrypt 哈希）、
  `edit`（转发前为请求追加 Header）、`mirror`（异步流量镜像）
- `forward`：转发引擎参数（连接池容量、连接/TLS/响应头超时、
  `request_timeout`、`stream_idle_timeout`、`response_body_timeout`、
  `streaming_flush`、`disable_compression`、`force_attempt_http2`、
  `buffer_size`）
- `server`：代理监听地址对应的 HTTP 服务器参数
- `log`：可选的文件日志与按大小滚动（未配置时输出到 stderr）
- `tls`：为代理自身的监听端口启用 TLS（支持 Let's Encrypt 的 `fullchain.pem`）

各字段的默认值、校验规则与调优建议见 [DEPLOY.md](DEPLOY.md)。

## 请求转发行为

- **方法**：HTTP 请求原样保留方法；`CONNECT` 建立原始 TCP 隧道
- **请求头**：双向剥离逐跳头，包括 `Connection` 头中列出的字段。客户端的
  `Accept-Encoding` 原样转发（正向代理保持透明）；`Proxy-Authorization` 仅由
  代理消费，不会转发给上游
- **请求体**：原样转发，并保留 `Content-Length`
- **重定向**：上游 3xx 响应原样返回客户端，代理不自行跟随
- **流式响应**：`text/event-stream` 响应逐块 flush
- **隧道**：HTTP/1.1 CONNECT 劫持为原始 TCP 隧道，HTTP/2 CONNECT 在流内转发；
  两者都使用 `forward.dial_timeout` / `forward.dial_keep_alive`，并复用引擎的
  缓冲区池
- **镜像**：`mirror` 开启时，普通 HTTP 请求在转发的同时异步投递一份到镜像目标；
  CONNECT 不镜像，投递失败仅在 `debug` 模式下记录日志
- **失败处理**：响应开始前转发失败返回 502 Bad Gateway；响应已经开始后的失败
  仅记录日志

## 内置工具

`tools/request-printer` 是一个配套服务，把收到的每个请求以 JSON 格式记录到日志，
便于查看代理实际转发到上游的内容（`make run-printer`）。它的配置说明以及
`make test` 集成测试见 [DEPLOY.md](DEPLOY.md#testing-utilities)。

## 项目结构

```
domain-forwarder/
├── main.go                      # 命令行参数、启动流程、优雅退出
├── config.go                    # 配置类型、默认值、加载与校验
├── pkg/
│   ├── proxy/proxy.go           # 正向代理处理器（CONNECT 隧道 + HTTP 转发）
│   └── forwarder/forwarder.go   # HTTP 转发引擎（连接池、超时、流式拷贝）
├── tools/request-printer/       # 调试用请求日志服务
├── config.example.yaml          # 带注释的配置模板（复制为 config.yaml）
├── Makefile                     # 构建自动化
├── scripts/                     # install.sh / uninstall.sh / systemd 服务文件
├── DEPLOY.md                    # 部署与运维指南
├── README.md                    # 本文档（中文）
└── README_EN.md                 # 英文文档
```

## 部署

Linux 一键安装（systemd 服务、独立运行用户）：

```bash
make build-linux
sudo ./scripts/install.sh
```

手动部署、服务管理、日志查看与故障排查见 [DEPLOY.md](DEPLOY.md)。
卸载：`sudo ./scripts/uninstall.sh`。

## 许可

MIT
