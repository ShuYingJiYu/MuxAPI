# MuxAPI ✕ kiro.rs 调度对照笔记

> 同类项目调度容错设计对照 ｜ 日期：2026-06-12
> 对照对象：[hank9999/kiro.rs](https://github.com/hank9999/kiro.rs)（Rust，⭐1655，Anthropic API → Kiro/CodeWhisperer 代理）
> 目的：印证 MuxAPI 设计决策，记录可反向借鉴的点。

## 0. 最本质区别：调度对象不同

| | kiro.rs | MuxAPI |
|---|---------|--------|
| 调度对象 | **同一上游(Kiro)的多个账号凭据** | **多个不同的上游中转站** |
| 凭证模型 | 一个 Kiro，N 个 refreshToken | N 个 `{base_url, api_key}` |
| 额外负担 | 需 token 刷新/回写（账号会过期） | 纯透传，key 不过期 |

> 二者是「一上游多账号」vs「多上游」的镜像问题。这个差异是后文所有处理分歧的根源。

## 1. 选源逻辑

| 维度 | kiro.rs | MuxAPI |
|------|---------|--------|
| 默认策略 | priority：选 `min(priority)`（`token_manager.rs:739`） | 严格优先级：只在最高优先层内选 ✓ 一致 |
| 同级分流 | balanced：least-used 成功数最少（`token_manager.rs:733`） | 同层 weightedPick 加权随机 + P2C 延迟加权(EWMA) |
| 高级路由 | ❌ 无 | ✅ 延迟加权选路 + 首字节 TTFT 切换 |

结论：MuxAPI 选路更聪明（P2C/EWMA），kiro.rs balanced 仅按成功次数计数。

## 2. 故障分级哲学（核心对照）

两边都做了同一件正确的事：**区分「凭证/账号问题」与「上游瞬态问题」**。

| 状态码 | kiro.rs | MuxAPI(`failScope`) |
|--------|---------|---------------------|
| 401/403 | 先 force-refresh 一次再计失败切号（`provider.rs:408`） | 熔整个上游（凭证级） |
| 402 额度用尽 | 禁用此号 + 切换（`provider.rs:364`） | 熔整个上游 |
| 429 限流 | **只退避重试，不切号**（`provider.rs:439`） | 熔该模型（模型级） |
| 408/超时 | 只退避 | 熔该模型 |
| **5xx** | **只退避，当瞬态** | **熔整个上游** |

### 关键分歧：5xx 的相反处理，两边都对

- kiro.rs 是「同一上游多账号」→ 502 是 Kiro 整体抽风，切账号无用 → 只退避是对的。
- MuxAPI 是「多个独立上游」→ A 站 502 那就换 B 站 → 熔断切走是对的。

> **同一个 5xx 因调度对象不同而处理相反，却都正确。这印证了 MuxAPI「多上游」定位下「5xx 熔断切走」决策的正确性。**

## 3. 回切能力（MuxAPI 完胜）

| | kiro.rs | MuxAPI |
|---|---------|--------|
| 回切机制 | priority 模式粘住 `current_id`，不主动回切（`token_manager.rs:774`） | 每请求重筛取最高优先层，A 恢复立即重选 ✓ |
| 恢复探测 | ❌ 仅「全灭自愈」被动重置（`token_manager.rs:792`） | ✅ HalfOpen 主动放行试探 + 主动探测 |

> **重磅**：kiro.rs priority 模式「命中 current_id 就一直用、不挂不换」——与 sub2api「不回切」是同一个毛病。MuxAPI「每请求重选 + 半开探测」恰好治好它。在最在意的 failback 上，MuxAPI 明确优于 kiro.rs。

## 4. 熔断精细度（MuxAPI 更强）

| | kiro.rs | MuxAPI |
|---|---------|--------|
| 健康状态 | 二态：启用/禁用 | 三态 CLOSED/OPEN/HALF_OPEN |
| 粒度 | 凭据级（+opus 过滤） | (上游, 模型) 双粒度 |
| 惊群防护 | ❌ | ✅ Claim 半开单名额闸门 |

## 5. 可反向借鉴 kiro.rs 的 3 点

1. **401 先 force-refresh 一次再切**（`provider.rs:408`）：纯透传用不上，但未来若接 OAuth 上游，「token 失效先刷新一次、每凭据仅一次机会」可直接抄。
2. **退避抖动**（`provider.rs:509`）：`200ms 翻倍封顶 2000ms + 随机 1/4 抖动`。若未来加「同上游瞬态重试」，抖动可防惊群。
3. **全灭自愈兜底**（`token_manager.rs:792`）：全熔断时自动重置试探一轮，而非直接 503。MuxAPI 现为 `len(tried)==0 → 503`，可考虑加「全灭时强制半开试探一遍」兜底。

## 总结

> MuxAPI 在「回切、熔断精细度、智能选路」三维度明确强于 kiro.rs（因专门针对 sub2api 回切痛点设计）；kiro.rs 强在 token 刷新生命周期（多账号场景的必需品，MuxAPI 纯透传用不上）。最有价值的洞察是 5xx 的相反处理，印证了 MuxAPI 多上游定位的正确性。
