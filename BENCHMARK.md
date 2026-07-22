# go-rmm 性能基线

> 基于 P3 + 性能优化（F1/F2）后的 benchmark 数据，作为性能回归基线。
>
> 环境：AMD Ryzen 7 PRO 4750U, Windows, Go 1.25.6
>
> 生成方式：`go test ./relay/ -run xxx -bench=. -benchtime=1s`

## 关键路径性能

| 操作 | 延迟 | allocs | 说明 |
|------|------|--------|------|
| **SaveTask** | 7.2ms | 97 | bbolt 写事务（含 fsync），是持久化安全保证 |
| **UpdateTaskStatus** | 6.9ms | 67 | 同 SaveTask（单写事务读-校验-写原子） |
| **GetTask** | 44µs | 18 | bbolt 读，无 fsync，快速 |
| **ListTasksByStatus（索引）** | 0.2ms¹ | 160¹ | F1 优化：按状态索引扫描，O(匹配数) |
| **PolicyCheck** | 4.3µs | 1 | F2 优化：快速预过滤跳过正则 |
| **EnvelopeMarshal(1KB)** | 9.8µs | 4 | 协议序列化 |
| **EnvelopeMarshal(100KB)** | 1ms | 4 | 大结果（截图 base64） |
| **CASignAgentCSR** | 3ms | 1092 | 证书签发（低频，每 Agent 24h 一次） |

¹ 真实场景：10000 completed + 10 dispatched，查在途（F1 前后对比见下）

## 优化记录

### F1: ListTasksByStatus 状态索引（55x 延迟提升）

**问题**：reaper 每 15s 调 `ListTasksByStatus(StatusDispatched, StatusAcknowledged)`，原全表扫描 + 逐条 Unmarshal，1000 任务 11ms，生产规模线性退化。

**优化**：新增 `bucketTasksByStatus` 索引（key=`status\x00taskID`），按前缀扫描。

**效果**（10000 completed + 10 dispatched 场景）：

| 指标 | 优化前（外推） | 优化后 | 提升 |
|------|--------------|--------|------|
| 延迟 | ~11ms | 0.2ms | **55x** |
| allocs | ~11000 | 160 | **69x** |

### F2: PolicyCheck 快速预过滤（28x 延迟提升）

**问题**：15 个正则每条命令全跑，120µs/op。

**优化**：每条正则配 `quickCheck` 小写子串，先 `strings.Contains` 预过滤，安全命令 O(1) 跳过全部正则。

**效果**：

| 指标 | 优化前 | 优化后 | 提升 |
|------|--------|--------|------|
| 延迟 | 120µs | 4.3µs | **28x** |
| allocs | 0 | 1（ToLower） | — |

## 不优化的项（工程判断）

### bbolt fsync（SaveTask 7.2ms）

**判断**：不改。fsync 是 at-least-once 可靠性地基——任务持久化后断电也不丢。

**容量分析**：
- 每任务全生命周期约 4 次写（SaveTask×2 + SaveResult + UpdateTaskStatus）= ~29ms
- 125 写/秒 ÷ 4 写/任务 ≈ **31 任务/秒**
- 内网 RMM 典型负载 < 5 任务/秒，远低于瓶颈
- 高并发（>30 任务/秒）才需考虑 batch 写 goroutine，但那应改架构非降 sync

### EventBus 广播

**判断**：不改。已持久化到 store，Console 可通过 `/api/tasks` 补拉。EventBus 只是实时推送通道，丢事件=延迟刷新非数据丢失。

## 性能回归门禁

CI 可用以下命令做性能回归检测：

```bash
# 跑 benchmark 并与基线对比（需 benchstat）
go test ./relay/ -run xxx -bench=. -benchtime=1s | tee new.txt
benchstat baseline.txt new.txt
```

关注指标：
- `ListTasksByStatusRealistic` 应保持 < 1ms（索引有效性）
- `PolicyCheck` 应保持 < 10µs（预过滤有效性）
- `SaveTask` 允许波动（fsync 受磁盘影响）
