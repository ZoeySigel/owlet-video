# 本地容量压测

已执行的结果见 [2026-09-23 容量报告](2026-09-23/report.md) 和 [原始测量汇总](2026-09-23/measurements.md)。其中包含失败档位，请勿只引用缓存命中读取的峰值。

针对热榜与 Outbox 的优化实现和后续复测见 [2026-09-25 优化报告](2026-09-25-optimized/report.md)。该轮通过 `compose.capacity.retest.yml` 使用新数据卷重新生成相对时间分布，避免旧互动自然过期后产生虚假的性能改善。旧卷与旧报告保留。

后续数据库往返优化、近时旧版对照及组提交实验见 [第二轮报告](2026-09-25-roundtrip/report.md)。该时段 100 RPS 纯异步仍持续积压，不能根据上一轮短测推断当前持续容量。

写入入口预算、积压保护、30 分钟持续测试与依赖故障验收见 [2026-09-26 工程验收报告](2026-09-26-steady/report.md)。该轮使用 `compose.capacity.soak.yml` 的独立数据卷。默认每 API 进程允许 20 RPS 互动写入；更高档位的拒绝属于保护行为，不能统计成成功吞吐。

只测试 `compose.capacity.yml` 的 `owlet-capacity` 项目，API 固定为 `127.0.0.1:28080`，数据库固定为 `127.0.0.1:23316/owlet_capacity`。压测工具拒绝覆盖非空的 fixture 数据库；没有生产地址参数。

## 准备

Windows PowerShell，在仓库根目录执行：

```powershell
New-Item -ItemType Directory -Force .tools/capacity | Out-Null
$env:GOOS='linux'
$env:GOARCH='amd64'
$env:CGO_ENABLED='0'
go -C backend build -trimpath -o ../.tools/capacity/owlet-video .
Remove-Item Env:GOOS,Env:GOARCH,Env:CGO_ENABLED
go -C backend build -o ../.tools/capacity/loadgen.exe ./tools/capacity
docker compose -f compose.capacity.yml up -d --wait api
.tools/capacity/loadgen.exe -mode seed
docker compose -f compose.capacity.yml up -d --wait worker
```

Linux 先编译同一 Linux API 二进制；压测工具命名为 `.tools/capacity/loadgen`，其余参数相同。所有命令都在仓库根目录执行。

Fixture 为 1 万用户、10 万视频元数据、60 万点赞、30 万评论、10 万关注。认证会话与测试 Access Token 预生成，只测已登录请求，不包含注册、bcrypt 登录或刷新吞吐。压测 Token 的测试有效期为两小时，业务服务的默认 Token 配置不改变。视频文件为占位路径，不测媒体下载和上传带宽。

## 执行

```powershell
python scripts/capacity/run.py --scenarios detail-hot,latest,detail-random,likes,hot,write,async,mixed --rates 20,100,300 --duration 30s --label screen
python scripts/capacity/run.py --scenarios mixed --rates 50 --duration 30m --label soak
```

第二条的速率应依据实测筛选，不代表预先保证能通过。工具拒绝覆盖同名结果；用不同 `--label` 区分轮次。

本轮稳态验证实际使用 `--scenarios async --rates 10 --duration 2m --label steady` 和 `--scenarios mixed --rates 100 --duration 10m --label steady`。开始前确认之前的命令和 Outbox 已排空；不要把继承积压的阶段标成独立基线。汇总命令：

```powershell
python -X utf8 scripts/capacity/summarize.py docs/capacity/2026-09-23
```

每阶段同时生成汇总 JSON、执行日志、Docker 资源采样 JSONL。调度器按固定到达时间发起 HTTP 请求，上限 512 个在途请求；槽不足或调度迟到超过 100ms 计为 `Dropped`，不会通过排队伪装成达到目标速率。`Planned = Sent + Dropped`，非 2xx、超时、无效响应均计为错误，429 单列。`SuccessRPS` 只统计在发压窗口内完成的成功响应。

- `detail-hot`：同一热门详情；`detail-random`：分散 ID，不局限于缓存命中。
- `latest`、`likes`、`hot`：各自的首屏接口；热榜应跨分钟观察新快照生成成本。
- `write`：同步响应的真实评论写入，按测试账号轮换。
- `async`：`Prefer: respond-async` 评论请求；HTTP 202 只计受理，同时记录完成情况。
- `mixed`：30% 热详情、20% 分散详情、20% 最新 Feed、20% 点赞榜、10% 评论。它不是包含所有功能的生产流量模型。

每 5 秒采样待发送 Outbox、未完成命令、失败命令与通知数。每阶段最多等待 30 秒排空积压，记录排空耗时；结束后查询命令持久化结果，统计 `created_at → completed_at` 的处理延迟，以及已受理的异步操作有多少真正完成。该延迟从服务器写入命令记录开始，非完整客户端端到端延迟。监控使用额外数据库连接，存在少量测量开销。

## 判定与边界

初步筛选目标：读取 P95 ≤ 200ms、P99 ≤ 500ms；同步评论 P95 ≤ 1s；非预期错误 < 0.1%；丢弃为 0；异步命令和 Outbox 不持续积压。混合场景需同时查看分接口延迟，不能只看平均值。异步受理和最终完成分别报告，不能用 202 的 RPS 宣称业务写入吞吐。

30 秒筛选只能定位候选档位与明显瓶颈；持续稳定容量需要更长稳态测试。压测器运行在宿主机，服务在同一物理机的 Docker Desktop 虚拟机，双方共享 CPU/内存/磁盘，结果不等同于独立压测机或生产容量。本环境未加入 HTTPS、反向代理、真实视频 I/O 和公网网络。

API 为 2 CPU/512 MiB，Worker 为 1 CPU/512 MiB，MySQL 为 2 CPU/1536 MiB（buffer pool 768 MiB），Redis 为 0.5 CPU/256 MiB（maxmemory 192 MiB），RabbitMQ 为 1 CPU/768 MiB。业务连接池与限流保持当前实现，不为得到高分而关闭限流。

## 清理

```powershell
docker compose -f compose.capacity.yml down
```

此命令保留专用 MySQL 数据卷，便于检查结果。只有确定不再需要压测数据、需要从空库重新 seed 时，才对同一专用配置执行 `down -v`。不要替换成开发或生产 Compose 文件。
