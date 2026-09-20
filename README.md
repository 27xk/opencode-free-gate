# opencode-free-gate

使用 Go 实现的 OpenCode 免费模型反代网关。网关从公共代理池选取可用代理，并支持按请求轮换 HTTP、HTTPS、SOCKS5/SOCKS5H 代理。

## 关键特性

- 每次代理尝试使用独立的 `http.Transport`，不共享故障连接。
- 流式请求默认 3 秒内拿不到响应头就取消当前尝试，整个代理链共享 10 秒总预算。
- 非流式请求自动转换：上游始终走 OpenCode 免费层要求的 `stream: true`，网关自动聚合 SSE 事件流并向非流式客户端返回标准的完整 JSON 响应。
- 自动适配 OpenCode 免费模型最新风控：请求体自动注入/补全 `tools` 数组（包含 `bash` 且数量 ≥2）与 `stream: true`，彻底解决 `403 FreeTierError` 限制。
- 上游请求携带真实 OpenCode 客户端指纹头：`User-Agent: opencode/1.18.31 ai-sdk/provider-utils/4.0.40 runtime/bun/1.3.14`（支持通过 `OPENCODE_USER_AGENT` 自定义）、`x-opencode-client: cli`、`x-opencode-project: global`。
- 会话与请求标识 100% 还原 OpenCode CLI 源码算法：生成带 12 位时间戳 Hex 与 14 位 Base62 字符的合规标识（请求为 `msg_` 升序，会话为 `ses_` 降序）。
- 会话缓存与亲和性：多轮对话首条消息映射并缓存到稳定会话 ID，同一会话优先固定同一代理出口（rendezvous 哈希亲和），故障时自动回退。
- `/v1/messages` 与真实 OpenCode 客户端一致，使用 `x-api-key` 认证并自动补齐 `anthropic-version`。
- 支持兼容常见免费模型别名（如 `muse-spark-1.3`、`big-pickle`）。
- 保留原有 Docker 镜像名、端口、路由和环境变量。

## API 路由

| 客户端类型 | 路由 |
|---|---|
| OpenAI | `/openai/v1/models`、`/openai/v1/chat/completions` |
| Anthropic | `/anthropic/v1/messages` |
| Codex | `/codex/v1/responses` |
| 健康检查 | `/healthz` |

模型列表每 60 秒从 OpenCode 官方上游动态刷新，提取 `-free` 免费模型（展示时自动去除 `-free`，请求时自动补齐 `-free`）并保留特殊的 `big-pickle`。请求中的展示名称与别名会自动映射回上游模型名称。

## 会话与请求 ID

网关为每个上游请求生成 OpenCode 协议要求的标识头，客户端无需自行构造：

- 客户端若传入合规的 `x-opencode-session`（`ses_` + 26 字符），直接保留使用。
- 其他显式会话标识（`x-session-id`、`conversation-id`、请求体 `conversation_id` 或 `metadata.session_id`）或对话首条消息，通过会话缓存映射为合规的 `ses_` 标识，保证多轮对话始终映射到同一会话。
- 每请求唯一生成合规的 `msg_` 标识，同一客户端请求在多级代理重试期间保持不变。

## Docker 部署

```bash
docker compose up -d
```

或直接运行镜像：

```bash
docker run -d \
  --name opencode-free-gate \
  --restart unless-stopped \
  -p 13339:13339 \
  -e PORT=13339 \
  -e PROXY_MODE=auto \
  ghcr.io/guji08233/opencode-free-gate:latest
```

从 Bun 版本升级时，无需修改现有生产环境变量或 Caddy 路由，只需发布并拉取新的同名镜像。

## 环境变量

| 变量 | 默认值 | 说明 |
|---|---:|---|
| `PORT` | `13339` | HTTP 监听端口 |
| `PROXY_MODE` | `auto` | `auto` 使用完整回退链；`custom` 从自定义代理开始 |
| `PROXY_ORDER` | 空 | 逗号分隔的回退顺序，取值 `public`、`zen`、`custom`（如 `custom,zen,public`）；设置后覆盖 `PROXY_MODE` 的默认顺序，省略的层会被跳过，直连始终作为最后兜底 |
| `SLOT_COUNT` | `5` | 公共代理槽位数，限制为 3–5；并发请求从不同槽位轮询开始 |
| `SLOT_RETRIES` | 槽位数 | 单请求最多尝试的公共代理数 |
| `CUSTOM_PROXIES` | 空 | 逗号分隔的代理 URL，支持 HTTP、HTTPS、SOCKS5/SOCKS5H |
| `CUSTOM_RETRIES` | `10` | 自定义代理重试数；`0` 表示按代理数量轮询一轮 |
| `ZENPROXY_RELAY` | `https://zenproxy.top/api/relay` | ZenProxy relay 地址 |
| `ZENPROXY_KEY` | 空 | ZenProxy API key；为空时跳过该层 |
| `ZENPROXY_RETRIES` | `5` | ZenProxy 尝试次数 |
| `FORCE_RELAY` | `0` | `1` 表示强制只走 ZenProxy |
| `PROXY_PROBE_TIMEOUT` | `8000` | 代理探活超时，毫秒 |
| `PROXY_REFRESH_MS` | `300000` | 公共候选池刷新间隔，毫秒 |
| `PROXY_FIRST_BYTE_TIMEOUT` | `3000` | 流式请求单次尝试取得响应头的最大时间，毫秒 |
| `HARD_TIMEOUT` | `10000` | 流式请求选择和重试链的总预算，毫秒 |
| `NON_STREAM_TIMEOUT` | `300000` | 非流式请求从进入网关到完整响应结束的最高时间，毫秒 |
| `OPENCODE_USER_AGENT` | `opencode/1.18.31 ai-sdk/provider-utils/4.0.40 runtime/bun/1.3.14` | 发往上游的 User-Agent |
| `TZ` | 系统默认 | 容器时区；镜像已包含 `tzdata` |

当前生产使用的 `CUSTOM_PROXIES`、重试次数和 ZenProxy 配置均可原样沿用。

## 重试规则

- 网络错误、连接/握手/首字节超时：关闭当前连接并切换代理。
- `401`、`403`、`408`、`425`、`429`、`5xx`：立即切换下一次尝试，不等待退避。
- 默认顺序为公共代理 5 次、ZenProxy 5 次、自定义代理 10 次，最后直连 1 次；`PROXY_ORDER` 可调整前三层的顺序或省略某些层，直连始终最后兜底。
- 代理层返回的 `429` 不会提前返回客户端；完成全部代理重试后，最终直连仍为 `429` 时才原样返回。
- 其他 `4xx`：视为业务请求错误，立即返回客户端。
- 流式总预算或非流式最高时间耗尽：返回 `504`，并取消仍在进行的底层请求。

## 本地开发

需要 Go 1.24 或更高版本：

```bash
go test ./...
PROXY_MODE=custom go run .
```

构建容器镜像：

```bash
docker build -t opencode-free-gate:local .
```

测试包含一个会接受 TCP 连接但永不返回数据的本地假代理，用于验证超时后连接确实被关闭。
