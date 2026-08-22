# MuxAPI 智能路由系统设计

> 日期：2026-08-23
> 状态：设计中
> 范围：将现有碎片化的路由逻辑统一为一个自洽的系统

## 动机

当前智能路由是逐个需求叠加出来的：cost model、cache 估算、模型映射、session 感知各自独立。
现实中这些是同一个决策的不同维度——一个请求到来时，系统需要在一次决策中同时考虑：

- 这个 session 的缓存在哪个渠道、还剩多久过期？
- 这个渠道的 cache 价格值不值得？（5min TTL vs 1h TTL）
- 如果最优渠道挂了，切到备选后成本怎么变？
- 这个模型名在目标渠道能用吗？

## 系统模型

整个路由可以用一个状态机描述：

```
请求到来
  │
  ├─ 1. 特征提取（model、session、prefix、stream）
  │
  ├─ 2. Session 状态查询
  │     └─ 这个 session 在哪些渠道有活跃缓存？还剩多少 TTL？重建过几次？
  │
  ├─ 3. 候选构建（每个 upstream 的 Candidate）
  │     ├─ 健康状态（breaker）
  │     ├─ 模型兼容性（含映射）
  │     ├─ 价格（input/output/cache_read/cache_write）
  │     ├─ 缓存画像（该 session 在此渠道的缓存状态）
  │     └─ 性能（延迟/可靠性）
  │
  ├─ 4. 成本预测（每个候选的 forecast window 成本）
  │     ├─ 无缓存路径：N × input_tokens × price
  │     ├─ 有缓存路径：考虑 TTL 过期次数、hit_rate、create 成本
  │     └─ 选更便宜的路径
  │
  ├─ 5. 选择 + 发送
  │     ├─ 模型名翻译（per-upstream）
  │     └─ 注入 cache_control 头（TTL 策略）
  │
  └─ 6. 观测记录（更新 session 缓存状态）
        ├─ cache_hit / cache_creation → 更新 expires_at
        ├─ 失败 → 更新 breaker + 可能触发模型映射学习
        └─ 供下次决策使用
```

## 核心概念

### Session Cache State（会话缓存状态）

一个 session 在一个 upstream 上的缓存生命周期：

```
COLD → WARMING → HOT → EXPIRED → COLD
                   ↑               │
                   └───── 重建 ─────┘
```

状态：
- **COLD**：该 session 从未在此 upstream 建过缓存（或已过期且无观测）
- **WARMING**：刚发了 cache_creation，缓存存在但尚未验证命中
- **HOT**：最近有 cache_hit 观测，缓存活跃
- **EXPIRED**：最后一次 cache_creation/hit 距今 > TTL

决策影响：
- HOT upstream 的成本估算用 cache_read price（便宜）
- COLD/EXPIRED upstream 的成本估算要加 cache_write price（贵）
- 如果 HOT upstream 挂了 → 切到次优，但标记为"需要重建缓存"

### 自适应 TTL 策略

Anthropic 支持两种 TTL：5min（默认，免费）和 1h（加价 ×4）。

决策逻辑：

```
session_duration = now - first_request_in_session
cache_rebuilds = count(cache_creation events in this session on this upstream)
avg_request_interval = session_duration / request_count

if session_duration > 10min AND cache_rebuilds >= 2:
    # 已经因为 5min TTL 断了两次 → 升级到 1h
    preferred_ttl = 1h
elif avg_request_interval > 4min:
    # 请求很稀疏，5min TTL 每次都会过期 → 1h 可能更划算
    # 但要 break-even 验证：1h 的额外成本 vs 多次 cache_write 的成本
    if cost_of_1h_ttl < cost_of_repeated_5min_creates(forecast):
        preferred_ttl = 1h
    else:
        preferred_ttl = 5min
else:
    preferred_ttl = 5min
```

### 缓存过期追踪

关键事实：**Anthropic 的 cache TTL 从 creation 时刻开始计算，不是从最后使用。**

每次观测到 `cache_creation_tokens > 0`：
```
expires_at = now + preferred_ttl
```

每次路由决策时：
```
remaining = expires_at - now
if remaining <= 0:
    state = EXPIRED
    # 成本估算要包含一次 cache_write
elif remaining < 30s:
    state = EXPIRING_SOON
    # 可以主动在本次请求里 refresh（发 cache_control 重建）
else:
    state = HOT
    # 成本估算只算 cache_read
```

### 故障转移后的成本重算

当 HOT upstream 断路器打开：

1. 该 upstream 被标记 `Healthy: false`，selector 跳过它
2. 剩余候选重新排序——它们大概率是 COLD 状态（从没给这个 session 建过缓存）
3. 成本估算自动包含 cache_write（因为没有 `Existing.Valid`）
4. 如果 forecast 显示请求量大 → 选一个缓存渠道重建
5. 如果 forecast 显示请求量小 → 选最便宜的无缓渠道
6. **当原 HOT upstream 恢复时**（断路器半开 → 关闭）：
   - 检查其缓存是否还在 TTL 内
   - 如果是 → 优先切回（已有缓存，成本低）
   - 如果过期 → 和其他渠道公平竞争

### 模型映射（已实现，融入框架）

位于候选构建阶段（step 3）：
- `SupportsModel` 判断时先走映射解析
- 发送请求时翻译模型名
- 失败时 RecordFailure → 自动学习
- 成功时 RecordSuccess → 清除错误映射

### 统一数据源

所有决策状态都从同一个地方来：

| 数据 | 来源 | 刷新 |
|------|------|------|
| 请求频率/forecast | `routing_observations` 按 session 聚合 | 每次请求后 |
| 缓存状态/hit_rate | `routing_observations` 按 session+upstream 聚合 | 每次请求后 |
| 缓存过期时间 | `upstream_prefix_cache_stats.expires_at` | 观测到 cache_creation 时更新 |
| 渠道健康 | breaker 内存状态 | 实时 |
| 模型兼容 | health + model_mappings | breaker 标记 / 自动学习 |
| 价格 | billing + LiteLLM catalog | 30s 缓存 |

### 不需要 Redis

所有状态都是**单实例范围**的（MuxAPI 是单 pod 部署）：
- 内存缓存（5s TTL）挡住热路径的 DB 查询
- SQLite 是持久化层（重启恢复）
- 观测数据写入是异步的（不阻塞请求）

## 当前差距（需要实现的部分）

| 组件 | 当前状态 | 目标状态 |
|------|---------|---------|
| Session cache state | 仅从 observations 事后聚合 | 主动追踪 per-session-per-upstream 的 COLD/HOT/EXPIRED |
| TTL 自适应 | 写死 5min | 根据 session 特征自动选 5min/1h |
| cache_control 注入 | 不注入 | forwarder 层根据 TTL 策略注入请求头 |
| 故障转移成本重算 | 已自动（selector 跳过 unhealthy） | 已满足（无额外工作） |
| 恢复后切回 | 自动（恢复后参与竞争，如果缓存还在就赢） | 已满足 |
| 缓存过期精确追踪 | 从 last_hit/last_create + 5min 推算 | 每次 create 时精确记录，考虑 TTL 类型 |

## 实现计划

### Phase 1：Session Cache State 精确化

目标：让路由决策时能准确知道"这个 session 在这个 upstream 的缓存还有多久过期"。

改动：
- `getSessionCacheStats` 返回时精确计算 `ExpiresAt`（基于 last_cache_creation + TTL）
- `intelligent.go` 的 `cache()` 方法传入正确的 `Existing.ExpiresAt`
- `cost.go` 的 `cacheLifetimes` 已经正确处理过期时间（已有）

### Phase 2：自适应 TTL

目标：session 持续时间长且多次重建缓存时，自动升级到 1h TTL。

改动：
- `getSessionCacheStats` 额外返回 `CreateCount`（该 session 重建次数）和 `FirstSeenAt`
- `intelligent.go` 的 `cache()` 方法根据 session 特征决定 TTL
- `CacheProfile` 新增 `PreferredTTL time.Duration` 字段
- forwarder 根据 `PreferredTTL` 注入 `anthropic-beta: prompt-caching-2024-07-31` 和
  请求体里的 `cache_control` block

### Phase 3：cost model 适配 1h TTL 价格

目标：break-even 计算考虑 TTL 类型的价格差异。

改动：
- `Pricing` struct 新增 `LongCacheWritePerToken`（1h TTL 的 write 价格）
- `EstimateWindowCost` 根据 `CacheProfile.PreferredTTL` 选用对应价格
- break-even 公式纳入"避免多次 5min cache_write"的收益

## 审计与可观测

每次路由决策的 JSON 审计日志（已加）包含：
- session 缓存状态（哪个渠道 HOT、剩余 TTL）
- TTL 选择理由
- 成本对比（cache vs no-cache vs long-cache）
- 实际命中/未命中结果（事后补全）

这让我们能从日志回放验证算法的每一步是否合理。
