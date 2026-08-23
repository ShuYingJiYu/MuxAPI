# MuxAPI 智能路由架构重构

## 背景

MuxAPI 是一个 LLM API 网关，核心功能是在多个上游渠道间做成本最优路由。当前 dev 分支（`9f21b91`）的智能路由功能已经在线跑，功能正确，但代码是 25 个 commit 逐个打补丁堆出来的，架构需要清理。

仓库：`~/Desktop/homelab/repos/MuxAPI`，dev 分支。
线上 test 环境：`mux-dev.sakurapuare.com`，流量持续导入中。

## 系统做什么

一个请求进来，路由系统在 8 个上游渠道里选成本最低的那个发出去。渠道分两类：
- **无缓存**（aws0ex/aws0sushua/aws0，multiplier 0.033-0.066）：便宜但每次全价计费
- **有缓存**（awsqaex/awsqasushua/awsqa/awsqsushua/awsq，multiplier 0.099-0.59）：贵但 prompt caching 让重复 prefix 只付 1/10 价格

路由的核心判断：**这个请求走缓存渠道是否比走便宜的无缓存渠道总体更划算？** 取决于：
- 请求频率（rpm 高 → 缓存复用次数多 → 划算）
- TTL 过期次数（5min TTL 在低频下反复过期重建 → 不划算）
- 缓存覆盖率（上游只缓存 ~77% prefix，不是 100%）
- 是否有现存缓存（HOT 状态不需要首次 create 开销）

## 当前架构问题

### 1. `intelligent.go` 是 god object
Pricing、cache profile、coverage ratio、billing fallback、forecast 全在一个文件的方法链里。每加一个数据维度就加一个 `func (r *intelligentRouter) xxx()` 方法，没有明确的数据流。

### 2. 成本模型的输入散落多处
- `multiplier`：billing_status → snapshots fallback → credit_ratio fallback（三层 if/else）
- `coverage_ratio`：单独一个 SQL 查询
- `hit_rate`：从 prefix_stats / session_stats 两种聚合路径
- `cache_existing`/`expires_at`：同上

这些互相不知道彼此的结果，每个都独立查 DB 再组装进 `CacheProfile` 和 `Pricing` struct。

### 3. CacheProfile 字段互相覆盖
`DefaultHitRate`、`CoverageRatio`、`HitRate`、`HitRateKnown` 四个字段的优先级关系只在 `cost.go` 的 if/else 链里体现。读 types.go 看不出最终用了哪个值。

### 4. 前后端成本单位不一致
后端：15 分钟窗口 N 个请求的总预测成本（用于比较）
前端：需要单请求成本（用于展示）
`/N` 的转换散落在 Vue 模板里，不在 API 层。

### 5. 探索逻辑和成本计算耦合在 selector 里
`chooseExploration` 需要读 `Price.Multiplier` 来过滤贵渠道——它需要知道成本模型的内部细节。

## 设计 spec（已有，供参考）

`docs/superpowers/specs/2026-08-23-intelligent-routing-system-design.md` 是之前对话产出的整体设计文档，描述了完整的状态机和数据流。实现偏离了这个 spec——应该回到它的框架。

## 重构目标

1. **三个正交维度分开**：定价（static）、缓存效率（observed）、流量形态（realtime），各自有独立的 data source 和计算逻辑，组合发生在最后一步
2. **每个 candidate 的成本估算是一个纯函数**：输入是三个维度的结构化数据，输出是成本数字。可以单元测试每种场景
3. **API 返回的数据直接可展示**：单请求成本、各候选对比、节省金额 —— 不需要前端做数学
4. **观测收敛有明确的状态机**：COLD→HOT→EXPIRED，coverage_ratio 和 hit_rate 是状态的属性，不是散落的字段

## 约束

- 不能停服。重构期间线上继续跑，用 feature flag 或渐进替换
- 所有现有 test case 必须继续通过
- 前端已有的页面（路由决策、日志详情）保持功能不回退
- `cmd/apigen` 生成的 API contract 继续使用

## 相关文件

关键代码（都在 `internal/`）：
- `routing/` — 纯逻辑层：cost.go（成本公式）、selector.go（选择器）、types.go（数据结构）、features.go（特征提取）、tracker.go（观测记录）
- `scheduler/intelligent.go` — 适配层：把 store/health 数据组装成 routing 层的输入
- `store/routing_observations.go` — 持久化：prefix cache stats、session 聚合
- `store/billing.go` — billing 状态和 snapshot 历史
- `store/route_decisions.go` — 路由决策持久化（审计）
- `forward/forward.go` — 转发层：发请求、观测结果、cache_control 注入
- `server/routing_audit.go` — 审计日志：把决策和结果写入 route_decisions 表

前端：
- `web/src/RoutingView.vue` — 路由决策页面
- `web/src/composables/useLogs.js` — 日志页辅助
- `web/src/api.generated.js` — 自动生成的 API contract

设计文档：
- `docs/superpowers/specs/2026-08-23-intelligent-routing-system-design.md`

## 会话上下文（前因）

1. 对话 `3bba0085` 做了初始设计和主要实现（intelligent routing + model mapping + mock tests + routing UI + adaptive TTL + failover）
2. 本对话 `b6120998` 修了：
   - 乐观先验 hit rate（0.85，让缓存渠道首次就能竞争）
   - billing multiplier 丢失（COALESCE + snapshot fallback）
   - cache coverage ratio（上游只缓存 77%，成本模型修正）
   - 探索只选成本接近的渠道（不浪费钱在 fallback 渠道上）
   - 前端字段映射错误 → apigen 体系化解决
   - 前端显示单请求成本而非窗口总成本
   - SPA 404、DB 同步覆盖 test 数据

每个修复都是正确的，但都是补丁。架构需要一次性理清。
