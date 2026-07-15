# MuxAPI

> AI API 多路复用调度转发网关 —— 严格优先级 · 渠道级熔断 · P2C 选路 · 自动回切

MuxAPI 是一个轻量的 AI API 中转调度网关。它把客户端请求按「分组 → 上游池」转发到多个上游中转平台，提供**严格优先级调度**、**渠道级熔断**、**标准 P2C 选路**、**故障自动回切**与 **per-upstream 代理出口**，并自带一个 Vue3 管理后台。

## 为什么

主流中转方案常见三个痛点：

- **优先级不严格**：加权随机，做不到「A 活着就必走 A」；
- **不回切**：高优先级上游恢复后不主动切回；
- **故障发现被动**：靠请求失败才感知，恢复也被动。

MuxAPI 针对这三点设计：严格优先级、主动探测发现故障与恢复、上游恢复后自动回切。熔断按上游渠道统一管理，模型差异只记录为短期能力缓存。

## 特性

- **多上游聚合**：全局上游池 + 分组隔离，每个分组是独立调度池，拥有自己的上游成员与接入密钥
- **严格优先级**：优先级数字越小越优先，绝不掺低优先级层
- **标准 P2C**：同优先级层内按权重独立抽取 2 次，比较渠道 TTFT EWMA 与当前并发
- **渠道级熔断**：不同模型的连接、鉴权、限流和上游错误共同计入同一渠道状态
- **模型能力缓存**：明确的模型不存在只短期排除该模型与渠道组合，不影响渠道健康
- **统一探测**：探测读取完整响应；流式响应必须出现完成事件，连续成功两次才恢复渠道
- **per-upstream 代理出口**：每个上游可单独配代理（`http` / `socks5`），轻松接入墙外上游
- **协议透传**：OpenAI（`/v1/chat/completions`、`/v1/responses`）、Claude（`/v1/messages`），原样转发
- **模型清单汇总**：`/v1/models` 实时汇总分组内各上游模型并集，带缓存
- **接入密钥**：客户端用接入密钥访问，按密钥路由到对应分组的上游池
- **监控看板**：为「上游 + 模型」配置监控项，成功率 / 延迟 / 24h 小时栅栏趋势，卡片可拖拽排序
- **Webhook 告警**：熔断状态翻转时推送 Webhook，带去抖防刷屏
- **请求审计**：每个客户端请求一条主记录，通过 `request_id` 关联完整渠道尝试链；区分 TTFT、总耗时、取消与流中断，默认保留 7 天
- **Web 管理后台**：分组、上游、密钥、监控、请求记录、运行时设置一站式管理；上游池支持状态筛选与拖拽排序

## 架构

五层解耦，策略可生长：

| 层 | 职责 |
|----|------|
| 接入层 Ingress | HTTP 入口、鉴权、协议透传、模型清单汇总 |
| 调度层 Scheduler | 按分组选上游：严格优先级 → 同层 P2C 延迟感知选路 |
| 健康层 Health | 渠道级熔断器 + 模型能力缓存 + Webhook 告警 |
| 转发层 Forward | 首字节前失败换源、首字节后透明流式转发、渠道尝试链 |
| 监控层 Monitor | 唯一主动探测源：双写看板统计与路由熔断器 |

**回切原理**：每个请求都重新筛选健康上游并取最高优先级层。高优先级上游一旦被健康层探测判定恢复，下个请求立即重新被选中——failback 自然发生，无需额外逻辑。

**流式切换边界**：首个响应字节到达前发生连接错误、失败状态或超时，会排除当前渠道并尝试下一优先级；首字节到达后改为透明转发，不解析或强制等待 SSE 完成事件，避免中转层误判正常响应。已经向客户端发送内容后不再换源，防止响应重复。

## 快速开始

前端通过 Go `embed` 内嵌进二进制，**构建顺序：先 build 前端，再 build 后端**。

```bash
# 1. 构建前端（产物 web/dist 会被内嵌进后端二进制）
cd web && npm install && npm run build && cd ..

# 2. 构建后端（单文件，已含前端 + 管理后台）
go build -o muxapi ./cmd/muxapi

# 3. 运行（可选：复制 .env.example 为 .env 修改端口/token 等）
cp .env.example .env
./muxapi
```

默认监听 `:8080`，启动前必须配置 PostgreSQL 连接串。浏览器打开 `http://<地址>:<端口>` 即管理后台，「设置」页会显示客户端接入地址。

应用启动时会按文件名顺序自动执行 `database/migrations/` 中尚未应用的 PostgreSQL 迁移，并记录到 `schema_migrations`。

从旧 SQLite 数据库迁移配置：

```bash
export MUXAPI_DATABASE_URL='postgres://muxapi:password@127.0.0.1:5432/muxapi?sslmode=disable'
go run ./cmd/migrate-sqlite -source ./muxapi.db
```

迁移工具会复制分组、渠道、成员关系、接入密钥、监控项与运行时设置；请求历史不迁移。

> `web/dist` 不存在时 `go build` 会因 embed 报错，务必先构建前端。开发模式（前端热更新）：`cd web && npm run dev`。

### 交叉编译

Go 主程序与 PostgreSQL 驱动均无需 cgo；SQLite 驱动仅供旧数据迁移和单元测试：

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o muxapi-linux-amd64 ./cmd/muxapi
```

产出静态链接二进制，丢到目标机直接运行。

## 配置

启动级配置通过环境变量，也可复制 `.env.example` 为 `.env` 写入（**真实环境变量优先于 `.env`**）：

| 变量 | 默认 | 说明 |
|------|------|------|
| `MUXAPI_ADDR` | `:8080` | 监听地址 |
| `MUXAPI_DATABASE_URL` | （必填） | PostgreSQL 连接串，建议连接本机加密隧道或私网地址 |
| `MUXAPI_TOKEN` | （空） | 管理后台鉴权 token，**留空则后台无鉴权，切勿对外暴露** |
| `MUXAPI_FAIL_THRESHOLD` | `3` | 连续失败多少次熔断 |
| `MUXAPI_COOLDOWN` | `30s` | 熔断冷却时长 |
| `MUXAPI_MAX_RETRIES` | `3` | 单次下游请求最多尝试的上游数 |

探测参数已全部**下放到各监控项**（探测周期、端点、消息内容、max_tokens、是否流式逐项可配），不再用全局环境变量。

以下为运行时设置，在管理后台「设置」页配置（存库、即时生效）：

| 设置 | 默认 | 说明 |
|------|------|------|
| `request_retention_days` | `7` | 请求记录保留天数，每 10 分钟分批删除过期请求与尝试链 |
| `alert_webhook` | （空） | 熔断翻转告警 Webhook URL，留空关闭 |
| `alert_debounce` | `60s` | 告警去抖窗口，同键窗口内最多发一次 |
| `route_smart` | `on` | 兼容保留项；调度固定使用标准 P2C |
| `first_response_timeout_ms` | `120000` | 首个响应字节超时，超时后切换渠道；流开始后不再施加应用层超时 |

## API

| 端点 | 协议 |
|------|------|
| `POST /v1/chat/completions` | OpenAI |
| `POST /v1/responses` | OpenAI Responses（兼容 Codex CLI 等） |
| `POST /v1/messages` | Claude |
| `GET /v1/models` | 汇总分组内各上游模型清单（OpenAI 兼容） |
| `GET /healthz` | 健康检查 |
| `/admin/*` | 管理 API（供后台调用） |

客户端用接入密钥访问（请求头 `Authorization: Bearer <access-key>` 或 `x-api-key`），MuxAPI 据此路由到对应分组的上游池并透传。

## 技术栈

- 后端：Go（标准库 `net/http`）+ PostgreSQL（`pgx`）
- 前端：Vue 3 + Vite + Chart.js

## 安全

- 生产环境务必设置 `MUXAPI_TOKEN`，否则管理后台无鉴权。
- PostgreSQL 连接应使用 TLS、私网或 SSH 隧道，禁止将应用账号直接暴露到公网。
- 数据库连接串、接入密钥和上游凭证不得提交到代码仓库。
