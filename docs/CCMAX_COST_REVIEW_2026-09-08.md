# CCMAX 成本复盘（2026-09-08）

## 结论

当天 `ccmax` 组共有 3,846 个请求：3,065 成功、669 个 `no_upstream`、67 个上游/超时失败、33 个取消。主要可用通道 C-API（upstream 19）承载了 3,150 次尝试，平均耗时约 17 秒，P95 约 44 秒。

按本地价格快照重算，已能定价的请求约为 495 个成本单位；其中缓存写入约 593 个单位，占标价估算约 78%。NewAPI 的账本快照当日增量约 871 个单位。两者不是可以直接相减的“多收钱”证据：账本还可能包含共享 key、时间边界和非 MuxAPI 流量；真正的问题是 MuxAPI 没有把每个路由决定与实际成本闭环起来。

## 数据暴露的三个教训

1. **缓存命中和缓存写入不是互斥事件。** 长对话中同一响应可同时包含 `cached_tokens` 与 `cache_creation_tokens`。旧模型用“未命中次数 × 当前前缀”估算写入，系统性漏掉滚动缓存写入；而 CCMAX 当天写入费是最大成本项。
2. **多轮会话必须按 session 聚合真实写入 token。** 每轮 prefix hash 都会变化，单 prefix 统计会退化成先验。现在 store 统计窗口新增写入次数和写入 token 均值，成本模型优先采用该观测，并保留 TTL 冷启动下限。
3. **不可用请求也要留下决策证据。** 669 个 `no_upstream` 原先没有 route decision，无法回答“为什么没有候选”。现在会持久化候选拒绝原因；重试后的每个 attempt 也绑定自己的候选价格，并在有完整用量时写入 `actual_cost`。

## 代码改进

- `internal/store/routing_observations.go`：窗口统计增加 `WindowCreateCount` / `WindowCreateTokens`，覆盖精确 prefix 和 session fallback。
- `internal/routing/cachestate.go`、`types.go`：将滚动写入率和每请求写入 token 传入 `CacheProfile`。
- `internal/routing/cost.go`：缓存写入按独立事件计价；当观测显示写入与命中重叠时，使用观测到的写入 token 率，并发出审计 warning。
- `internal/routing/selector.go`：低样本失败使用虚拟成功率惩罚，避免 0% 但样本不足的通道被长期当作“未知”优先选择。
- `internal/server/routing_audit.go`、`internal/forward/forward.go`、`internal/scheduler/intelligent.go`：保存无候选决策、按 attempt 绑定决策、回写实际成本。

## 后续观测

- 持续比较 `route_decisions.actual_cost` 与 provider 账本快照；不能再只看 HTTP 200 或账本总额。
- 给 upstream 增加主动健康探测。当天 669 个 `no_upstream` 没有候选拒绝记录，且现有 monitor 数为 0，说明可用性主要依赖真实流量触发。
- 账单状态表在双向复制下可能比最新 snapshot 陈旧；运行时价格应优先使用最新成功 snapshot，或明确单写者，避免“状态新鲜度”被误判。
