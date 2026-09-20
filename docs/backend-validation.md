# 后端升级验收

## 本次结果

2026-09-20 在 Windows、Go 1.27.1（项目最低 Go 1.24）和独立 Docker 测试环境执行。依赖为 MySQL 8.4、Redis 7、RabbitMQ 4.1；前端使用项目锁定依赖。没有连接生产数据库或中断现有开发容器。

- `go vet ./...`：通过。
- `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath`：Linux 生产目标编译通过。
- `go test -race -count=1 -v ./...`：14 项测试全部通过，包含真实依赖集成与连接故障注入，约 38 秒。
- 最后补充的缓存元数据损坏回退修复已通过带 `-race` 的针对性回归测试。
- `npm run typecheck`：通过。
- `npm run build`：通过，静态页面导出正常。首次在受限沙箱中因子进程 `spawn EPERM` 中断，在允许启动子进程的环境重跑成功。

| 验证内容 | 结果与观察 |
| --- | --- |
| 既有业务链路 | 邀请注册、分片上传、发布、列表、点赞、评论、关注、私信、异步通知通过；重复事件不重复写通知，无效消息进入死信 |
| 滑动窗口 | 窗口内点赞计 3 分、评论计 5 分；过期点赞退出，已删除评论退出，无新事件时窗口仍自然清空 |
| 稳定排名分页 | 55 条同分视频按 ID 打破平局；翻页中改变点赞数，结果仍按原快照顺序，无重复、无遗漏 |
| Redis 断线分页 | 后续页从 MySQL 快照读取；恢复并删除测试自己的榜单缓存后，请求重建 ZSET；游标过期返回 410 |
| 热点保护 | 两个 App 实例并发请求同一冷缓存详情共 100 次，只触发 1 次详情回源（含作者预加载，不等于只有 1 条 SQL） |
| 锁与空值缓存 | 不存在的视频不重复回源；撤销锁后旧令牌无法回填，旧持有者无法删除新锁 |
| 资源保护 | 数据库失败或回源槽已满时使用短期旧详情；无旧值且槽已满则拒绝请求；本地缓存与限流键数有上限 |
| Redis 故障恢复 | 熔断后单次半开探测恢复；通知仍落库且重复事件不增加通知；限流不在故障切换时重置本地预算 |
| RabbitMQ 故障恢复 | 测试代理断开当前连接，期间写入 Outbox，恢复后 Worker 自动重连、确认发送并消费积压事件 |
| SSE 兜底 | Redis 持续不可用时仍建立 SSE，并在初始提示后约 20 秒再次提示刷新 |
| 游标与并发安全 | 签名篡改、跨排序复用、错误偏移和过期校验；请求取消不取消共享回源；Go race detector 未报告竞争 |

故障注入使用测试进程自己的 TCP 代理，只切断测试连接，不停止公共服务。旧详情兜底通过关闭独立测试数据库连接池模拟数据库不可用；它不代表主从切换或整库恢复测试。

## 复现

先在仓库根目录启动独立依赖，端口为 MySQL 23306、Redis 26379、RabbitMQ 25672：

```powershell
docker compose -f compose.test.yml up -d --wait
$env:TEST_MYSQL_DSN = 'owlet_test:test-password@tcp(127.0.0.1:23306)/owlet_video_test?parseTime=True&charset=utf8mb4&loc=UTC'
$env:TEST_REDIS_ADDR = '127.0.0.1:26379'
$env:TEST_REDIS_PASSWORD = ''
$env:TEST_RABBITMQ_URL = 'amqp://owlet_test:test-password@127.0.0.1:25672/'
Set-Location backend
go vet ./...
go test -race -count=1 -v ./...
Set-Location ..
docker compose -f compose.test.yml down -v
```

Linux/macOS 可使用同样的 Compose 命令，并通过 `export TEST_MYSQL_DSN='...'` 等方式设置相同变量。`-race` 需要可用的 C 工具链。GitHub Actions 已配置使用真实 MySQL、Redis、RabbitMQ 执行 `go test -race`，本轮新增测试会随代码提交纳入该流程；上述结果来自本地执行。

测试会写入账号、互动和 Outbox 等数据；必须使用独立测试依赖。该 Compose 使用单独项目名和临时 MySQL 数据，`down -v` 仅清理这个测试项目。每次完整验收建议重新创建环境，避免既有注册限流计数和历史榜单快照影响测试。

## 未覆盖与适用边界

本次没有执行生产部署、生产故障演练、大规模持续压测或多节点故障转移。因此不提供生产 QPS、P99、可用性百分比或吞吐提升倍数。100 个并发请求验证的是热点合并行为，不是容量结论。

窗口排名仍使用 MySQL 聚合原始互动记录；默认榜单前 2000 条、每分钟刷新、快照约 14–15 分钟有效。缓存与通知提示是最终一致，降级限流按进程计算。前端完成类型检查与静态构建，但本轮没有重新进行全页面视觉验收，也未在真实浏览器等待 15 分钟验证过期提示交互。

生产上线前应执行正常发布流程，核对索引迁移耗时、API/Worker 配置一致性与 Caddy 健康路由。单机部署不能表述为数据库/主机级高可用集群。
