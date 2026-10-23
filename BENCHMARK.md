# go-rmm 性能基线

> v3.9.0 · 基于 go test -bench 基准测试 + 实际端到端验证
>
> 环境：AMD Ryzen 7 PRO 4750U (8C16T), Windows 11, Go 1.25.6

---

## 1. 核心基准(go test -bench)

### Buffer 池对比(sync.Pool vs make)

| 测试 | 耗时 | 内存 | 分配 | 提速 |
|------|------|------|------|------|
| make 256KB | 227μs | 262KB | 1 | 基线 |
| **pool 256KB** | **117ns** | **24B** | **1** | **1940x** |
| make 32KB | 0.8ns | 0B | 0 | 编译器栈优化 |
| pool 32KB | 165ns | 24B | 1 | pool 反而慢(编译器已优化) |

**结论**:256KB buffer pool **1940x 加速**,32KB 不需要 pool。

### IO 性能

| 测试 | 吞吐 | 内存 |
|------|------|------|
| io.Copy 1MB | 5.8 GB/s | 32B |
| pooledCopy 1MB | 2.6 GB/s | 57B |

**结论**:Go 标准库 io.Copy 已极优(编译器栈分配 32KB buffer)。pool 仅在 256KB 场景有效。

### 命令策略

| 测试 | 耗时 | 内存 | 说明 |
|------|------|------|------|
| CheckCommand(safe) | **1.6μs** | 64B | 安全命令 O(1) 跳过 33 个正则 |
| CheckCommand(blocked) | 1.7μs | 147B | 拦截也只需 1.7μs |

**结论**:quickCheck 预过滤有效,安全命令 33 个正则全部跳过。

### 二进制帧

| 测试 | 耗时 | 吞吐 | 内存 |
|------|------|------|------|
| BuildBinaryFrame 256KB | 128μs | 2.0 GB/s | 270KB |
| **ParseBinaryFrame 256KB** | **76ns** | **3.5 GB/s** | **16B** |

**结论**:解析零拷贝(直接切 slice), 极快。

---

## 2. WS 写队列优化(O5)

### 问题
writeMu 粗粒度锁:心跳+任务结果+进度+shell 输出并发写同一连接,锁竞争。

### 方案
BatchWriter:单 goroutine + channel 队列(无锁串行化)。

| 模式 | 锁竞争 | 延迟 |
|------|--------|------|
| writeMu(旧) | 多 goroutine 竞争 | 随并发线性增长 |
| BatchWriter(新) | **零锁竞争** | channel 投递 ~50ns |

---

## 3. bbolt 批量写(O6)

### 问题
每次 SaveTask = 1 个写事务 = 1 次 fsync(8ms),瓶颈 ~125 写/s。

### 方案
BatchStore:攒批(channel 256 缓冲,50ms / 64 条触发批量提交)。

| 模式 | fsync 次数 | 写入吞吐 |
|------|-----------|----------|
| 单条事务(旧) | N 次 | ~125 写/s |
| 攒批(新) | N/64 次 | **~8000 写/s** |

---

## 4. 端到端性能(实测)

### 任务执行延迟

| 操作 | 延迟 | 说明 |
|------|------|------|
| task run(Get-Date) | **1.1-2.5s** | 含 PowerShell 启动(~1s)+ WS 往返 |
| task stream(首字节) | **1.2s** | SSE 实时推送 |
| file upload 17B | **<1s** | SHA256 双边校验 |
| file upload 1MB | **~3s** | stream-through 256KB 帧 |
| net check | **1.5s** | TCP 连通性检测 |
| events 事件延迟 | **<100ms** | SSE 推送(agent 活动→CLI) |

### 并发能力

| 指标 | 数值 |
|------|------|
| Agent 并发连接 | 受 fd 限制(默认 65536) |
| 并发任务(Agent 侧) | pool 大小控制(默认 4) |
| EventBus 订阅者 | 无限制(channel 64 缓冲) |
| SSE 并发 | 无限制(per-write deadline 30s) |

---

## 5. 优化记录

| 日期 | 版本 | 优化 | 效果 |
|------|------|------|------|
| v1.0 | 状态索引(F1) | ListTasksByStatus 从全表扫描→索引前缀扫描 | **55x** |
| v1.0 | 策略预过滤(F2) | CheckCommand 加 quickCheck 子串预过滤 | **28x** |
| v3.9 | 256KB buffer pool(O2) | 文件传输热路径 make→pool | **1940x** |
| v3.9 | BatchWriter(O5) | WS 写锁竞争→channel 队列 | 零锁竞争 |
| v3.9 | BatchStore(O6) | bbolt 攒批事务 | **64x 写入** |
| v3.9 | CLIXML 过滤(P2) | PowerShell 输出噪音过滤 | 用户体验 |

---

## 6. 回归基线

以下数据作为性能回归基线,CI 中 `go test -bench` 对比:

```
BenchmarkPool256K-16          117ns     24B     1 alloc
BenchmarkPolicyCheckSafe-16   1.6μs     64B     1 alloc
BenchmarkParseBinaryFrame-16   76ns     16B     1 alloc
```

CI 回归阈值:任何基准 **慢 2x 以上** 标记为性能回归。

## 传输引擎基准(v4.7.5,2026-09-18,16 核 Win/i7)

| 基准 | 吞吐 | 结论 |
|------|------|------|
| BenchmarkFrameBuildParse(4K~1M 五档) | 2.0~3.3 GB/s | 帧封装纯开销可忽略,帧尺寸调优无收益 |
| BenchmarkSHA256Chunk(256K) | 1.29 GB/s | 增量 hash 非瓶颈 |
| BenchmarkStreamDownloadCore(8M/轮,读+hash+进度+限速查询) | 843 MB/s | 下载引擎核心远超千兆线速 |
| BenchmarkReceiveUploadCore(256K 块,含临时文件创建+写盘) | 202 MB/s | 落盘路径为引擎最低环,仍 ~2x 千兆 |

**调优结论**:引擎各环节均远超千兆线速(~110MB/s),256KB 帧尺寸与现有缓冲保持不变——
瓶颈在网络传输层,已由 Phase 1 直连(内网线速)与 Phase 2 数据面分离(多 TCP 并行)
在架构层解决;引擎侧无进一步调优空间,数据背书。
