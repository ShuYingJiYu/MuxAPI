# MuxAPI

> AI API 多路复用调度转发网关 —— 严格优先级 · 主动健康探测 · 自动回切

MuxAPI 是一个轻量的 AI API 中转调度网关。它把客户端请求按「分组 → 上游池」转发到多个上游中转平台，提供**严格优先级调度**、**主动健康探测 + 熔断**、**故障自动回切**与 **per-upstream 代理出口**，并自带一个 Vue3 管理后台。

## 为什么

主流中转方案常见三个痛点：

- **优先级不严格**：加权随机，做不到「A 活着就必走 A」；
- **不回切**：高优先级上游恢复后不主动切回；
- **故障发现被动**：靠请求失败才感知，恢复也被动。

MuxAPI 针对这三点设计：严格优先级、主动探测即时发现故障与恢复、上游恢复后自动回切。

## 特性

- **多上游聚合**：全局上游池 + 分组隔离，每个分组是独立调度池，拥有自己的上游成员与接入密钥
- **严格优先级 + 权重分流**：优先级越小越优先，同级按权重分流
- **主动健康探测 + 熔断**：周期探测上游，连续失败自动熔断，冷却后自动回切
- **per-upstream 代理出口**：每个上游可单独配代理（`http` / `socks5`），轻松接入墙外上游
- **协议透传**：OpenAI（`/v1/chat/completions`、`/v1/responses`）、Claude（`/v1/messages`），原样转发
- **接入密钥**：客户端用接入密钥访问，按密钥路由到对应分组的上游池
- **监控看板**：为「渠道 + 模型」配置监控项，主动探测成功率 / 延迟 / 趋势
- **Web 管理后台**：分组、上游、密钥、监控、运行时设置一站式管理
- **调用日志**：记录每次转发的上游、状态码、延迟

## 架构

四层解耦，策略可生长：

| 层 | 职责 |
|----|------|
| 接入层 | HTTP 入口、鉴权、协议透传 |
| 调度层 Scheduler | 按分组选上游：严格优先级 → 同级权重 |
| 健康层 Health | 主动探测器 + 熔断器（失败熔断 / 冷却回切） |
| 转发层 Forward | 透传上游、失败重试、调用日志 |
| 监控层 Monitor | 看板探测：成功率 / 延迟 / 趋势 |

## 快速开始

### 后端

```bash
go build -o muxapi ./cmd/muxapi
./muxapi
```

默认监听 `:8080`，数据存本地 SQLite（`muxapi.db`）。

### 前端（管理后台）

```bash
cd web
npm install
npm run build      # 生产构建，产物在 web/dist
# 或开发模式
npm run dev
```

## 配置

全部通过环境变量配置（探测间隔还可在管理后台「设置」页运行时调整，页面值优先）：

| 变量 | 默认 | 说明 |
|------|------|------|
| `MUXAPI_ADDR` | `:8080` | 监听地址 |
| `MUXAPI_DB` | `muxapi.db` | SQLite 路径 |
| `MUXAPI_TOKEN` | （空） | 管理后台鉴权 token，**留空则后台无鉴权，切勿对外暴露** |
| `MUXAPI_PROBE_INTERVAL` | `15s` | 路由健康探测间隔 |
| `MUXAPI_PROBE_MODEL` | `gpt-5.5` | 探测用模型 |
| `MUXAPI_PROBE_PATH` | `/v1/chat/completions` | 探测端点 |
| `MUXAPI_FAIL_THRESHOLD` | `3` | 连续失败多少次熔断 |
| `MUXAPI_COOLDOWN` | `30s` | 熔断冷却时长 |
| `MUXAPI_MAX_RETRIES` | `3` | 单次请求最大重试次数 |
| `MUXAPI_MONITOR_INTERVAL` | `5m` | 看板监控探测间隔 |

## API

| 端点 | 协议 |
|------|------|
| `POST /v1/chat/completions` | OpenAI |
| `POST /v1/responses` | OpenAI Responses（兼容 Codex CLI 等） |
| `POST /v1/messages` | Claude |
| `GET /healthz` | 健康检查 |
| `/admin/*` | 管理 API（供后台调用） |

客户端用接入密钥访问（请求头 `Authorization: Bearer <access-key>` 或 `x-api-key`），MuxAPI 据此路由到对应分组的上游池并透传。

## 技术栈

- 后端：Go（标准库 `net/http`）+ SQLite
- 前端：Vue 3 + Vite + Chart.js

## 安全

- 生产环境务必设置 `MUXAPI_TOKEN`，否则管理后台无鉴权。
- 接入密钥与上游凭证以明文存于本地 SQLite，请妥善保护数据库文件与部署环境。
```
