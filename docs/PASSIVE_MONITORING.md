# 被动监控

MuxAPI 的被动监控只观察已经发生的真实客户端请求和上游尝试，不会为了监控而向上游发送请求。

## 两种口径

- 主动监控：`internal/monitor` 定时发最小请求，结果写入 `probe_results`，用于判断没有流量时的可用性。
- 被动监控：请求完成后消费 `forward.Result`，请求级数据进入 `requests`，尝试级数据进入 `request_attempts`，进程内指标由 `internal/observability` 暴露。

被动观察器不会再次调用健康管理器，因此不会重复计入熔断、恢复或 Webhook 告警。一次用户请求只计一次；其中的每次上游尝试单独计数。切换后成功应表现为“请求成功 + 至少一次失败尝试”，而不是两个用户失败/成功请求。

## 端点

- `GET /admin/passive`：进程启动以来的累计快照。进程重启会清零；适合快速看实时进程状态。
- `GET /admin/passive/history?window=24h`：从请求审计表读取持久化窗口，返回请求级统计、明确的 `request_health` SLI 分母以及按 upstream/model 聚合的尝试统计。支持 `h`、`m`、`s` 和 `d`（最长 30 天）。
- `GET /admin/routing/calibration?window=24h&group_id=1&model=example`：只读成本审计比较，需 admin 鉴权；窗口默认 24 小时、最长 30 天，`group_id`（正整数）与 `model` 均可省略，组合时取交集。
- `GET /metrics`：独立 metrics listener 上的低基数 Prometheus exposition。业务 `:8080` 不注册这个路由。

## 结果分类

请求成功率只使用最终请求结果 `success`。`request_health` 将 `partial`、`failed`、`unavailable` 和未知结果计入失败分母；`canceled`、`client_error`、`unsupported` 单独展示并排除出上游健康分母。未知结果归入 `unknown`，不能默认当成功。响应中的 `requests` 保留既有日志统计口径，做被动 SLI/告警时以 `request_health` 为准。

Prometheus 指标只使用固定的 outcome/status 标签，不把 request ID、session、cache key、完整错误文本或请求内容放入标签。需要 upstream/model 维度时使用 history API 或请求日志查询。

## 路由成本审计比较

`comparison_basis` 固定为 `selected_candidate_window_average_vs_usage_cost`。比较双方为：

- `F`：选中候选的 `forecast_total_cost / forecast_requests`，即未经成功率惩罚的窗口平均成本。不能用决策的 `selected_cost`，后者可能包含风险调整。
- `A`：决策的 `actual_cost`，由单次请求 usage 和记录的定价快照计算，属于审计估算，不是上游扣款凭证。

窗口请求数会影响首次缓存写入与 TTL 摊销；`F` 不保证等于当前单次请求的预测成本。因此这些误差不应被称为单次预测精度或账单准确率，也不用于回填历史缺失成本。

筛选按决策 `created_at` 执行，不按完成时间。只有最终结果为 `success`、实际与选中上游为同一个正 ID、匹配候选同时为 selected/eligible、两项成本非 NULL 且非负、预测请求数大于零的决策进入比较。已知零成本有效；缺失候选、未知成本/上游、非成功请求和 failover 不进入误差汇总，不从 `selected_cost` 猜测补值。

| 字段 | 口径 |
|---|---|
| `total_decisions` | 时间、分组和模型筛选后的全部决策，含未完成及缺失数据 |
| `samples` | 可比较样本数 |
| `excluded_samples` | `total_decisions - samples` |
| `failover_samples` | 两个上游 ID 均已知且不同的决策数，是排除样本的子集，不要求成本已知或结果成功 |
| `forecast_cost_sum` / `actual_cost_sum` | 仅可比较样本的 `ΣF` / `ΣA` |
| `absolute_error_sum` / `mean_absolute_error` | `Σabs(A-F)` / `Σabs(A-F)/samples` |
| `underestimate_rate` / `overestimate_rate` | `A>F` / `A<F` 的样本比例，成本相等不进入任一方向 |
| `percent_samples` | 可比较样本中 `A>0` 的数量 |
| `mean_absolute_percent_error` | `mean(abs(A-F)/A)`，只使用 `A>0` 的子集；返回比例，`1` 表示 100% |

没有可比较样本时，各总和为 0，MAE 和高低估比例为 JSON `null`。没有正 usage 成本时，MAPE 为 `null`；`A=0` 仍进入绝对误差及方向比例，不能用来稀释 MAPE 分母。PostgreSQL 查询采用 `DOUBLE PRECISION`，避免将 float64 成本强制降为 `REAL` 单精度。


## 部署边界

生产环境将 `MUXAPI_METRICS_ADDR` 绑定到独立端口（默认 `:9090`），只创建 ClusterIP metrics Service，不把该端口加入业务 LoadBalancer 或 Ingress。Home 集群由 ServiceMonitor 抓取，并加载只在有真实流量时计算的失败率、延迟和审计丢弃告警；US 集群先保留内部 Service，跨集群抓取必须单独验证 DNS、TCP 可达性和 Prometheus 归属。

进程指标只属于当前 MuxAPI Pod，重启或滚动更新会重置；history API 查询的是该实例配置的审计数据库。实现没有新增聚合表，也不会把 Home/US 数据自动合并或去重；若现有数据库复制链路把审计行复制到另一侧，窗口会按复制后可见的 `created_at` 记录统计，复制范围和延迟仍以现有数据库复制契约为准。
