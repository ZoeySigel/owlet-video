# 后端亮点对照与使用

对照基准：[feedsystem_video_go 的亮点解析](https://github.com/LeoninCS/feedsystem_video_go/blob/main/feedsystem_video_go项目设计.md)。下面对应其 21 项亮点，描述 Owlet 的实际代码与使用边界。接口路径、存储模型和默认参数不要求与参考项目逐字相同。

| 项目 | 实现位置 | 当前行为 |
| --- | --- | --- |
| 鉴权缓存自愈 | `backend/session_cache.go` | Redis Session 缓存优先，未命中或故障回源 MySQL 并回填；绝对租期限制旧值寿命 |
| 双 Token 与撤销 | `backend/auth.go` | Access 15 分钟、Refresh 7 天；刷新轮换 Refresh；登出撤销当前会话，改名撤销旧会话并签发新会话，改密撤销全部会话 |
| 分布式锁防击穿 | `backend/cache.go`、`backend/timeline.go` | 详情与时间线通过带令牌的 Redis 锁合并重建；跨实例竞争等待，旧持有者不能释放新锁 |
| Feed 冷热分离 | `backend/timeline.go` | 最新、关注 Feed 的最近 1000 个 ID 缓存在 Redis ZSET；跨热数据边界时拼接 MySQL 冷数据；详情经 L1/Redis/MySQL |
| 滑动窗口热榜 | `backend/hot_buckets.go` | 实时增量按原互动分钟分桶，以事件 ID 去重；新快照在单条 SQL 内按视频聚合、排序并截取前 K 条，再缓存到 Redis |
| 主动失效 | `backend/cache.go`、`backend/interactions.go` | 互动清除详情与回填锁；改资料、头像清除作者视频缓存；发布和关注变化推进时间线版本 |
| 分片与断点续传 | `backend/video.go`、`backend/upload_cache.go` | 5 MiB 分片，整文件及分片 MD5；同用户、同 hash/大小/分片数复用未发布会话；Redis 缓存会话与 hash 索引，MySQL/磁盘兜底 |
| 话题标签 | `backend/video.go` | 标题和描述同时提取标签、规范化并去重，话题列表可查询 |
| SSE 通知 | `backend/worker.go`、`backend/messaging.go` | 通知先落库，再通过 Redis Pub/Sub 提示；20 秒定期刷新补偿丢失信号，支持列表/未读/已读 |
| 双字段游标 | `backend/likes_cursor.go` | `GET /api/v1/videos?sort=likes&pagination=keyset` 按点赞数、ID 倒序；保持原始 int64 精度 |
| 快照稳定分页 | `backend/ranking.go` | 默认热门和点赞榜使用持久化不可变快照及签名游标，Redis 故障不改变已发出游标的顺序 |
| 软硬鉴权 | `backend/session_cache.go` | Feed 无凭证允许匿名，携带非法/撤销凭证返回 401；写接口强制登录；兼容 Cookie 和 Bearer Access Token |
| Redis 限流 | `backend/auth.go`、`backend/main.go` | 登录/注册按 IP；点赞及取消共用每账号 30 次/分钟，评论及删除共用 10 次/分钟，关注及取关共用 20 次/分钟；Redis 故障保留本地预算 |
| 多级降级 | `backend/resilience.go` 等 | 缓存超时熔断、MySQL 回源、短期旧详情、限流本地预算、SSE 定时提示；恢复后请求自动重建 |
| MQ 异步业务写入 | `backend/interactions.go` | API 持久化命令与 Outbox，发布到 RabbitMQ，Worker 写点赞/评论/关注及计数；可选择立即返回 202 |
| 发布时间线 Outbox | `backend/video.go`、`backend/worker.go` | 视频与发布事件同事务，消费后推进时间线版本并重建热索引；API 提交成功也立即失效旧版本 |
| MQ 故障直写 | `backend/interactions.go` | 发布失败或兼容模式等待超时，API 锁定同一命令行执行；晚到 Worker 不重复写入；热度同时支持幂等直接更新 |
| Compose 部署 | `docker-compose.yml` | MySQL、Redis、RabbitMQ、API、Worker、前端及持久化目录 |
| 启动脚本与独立运行 | `start.sh`、`start.ps1` | 支持排除前端/Worker、退出时停止本次选定服务；仍可直接用二进制的 api/worker 模式 |
| 自动迁移 | `backend/main.go` | GORM 自动迁移业务表、命令表、Outbox、消费去重及快照表 |
| 健康检查与 pprof | `backend/profiling.go` | `/livez`、`/healthz`；可选独立回环监听 API 6060 / Worker 6061，公共路由不暴露 pprof |

## 消息处理接口

现有前端保持原响应结构：API 确认投递后最多等待 Worker 700ms，未完成则执行幂等直写。成功响应代表业务数据已经提交。API 复用固定四路独立发布连接，确认后尝试标记命令 Outbox 以减少重复投递；连接失效后下次请求重建。命令行锁、业务写入、结果与通知 Outbox 在同一事务，重复事件或超时直写不会重复增加计数。

需要异步接收语义时，互动写请求加入 `Prefer: respond-async`。成功投递返回 HTTP 202、`operationId` 与 `Location: /api/v1/interactions/<id>`。客户端通过该地址读取状态；仅命令所属用户可读取。最终响应包含 `httpStatus` 与 `result`。202 仅代表受理，最终业务校验可能返回失败；broker 投递失败仍走直写。默认模式返回 `X-Interaction-Execution: worker` 或 `fallback` 便于联调。

业务命令和通知均保留 Outbox；通知消费仍有手动 ACK、重试和死信。当前没有对历史命令/Outbox 做自动删除，避免误删尚可能重投的去重记录；长期运行需要根据积压与审计保留周期制定归档策略。HTTP 客户端重新发起的两个 POST 是两个命令；消息去重不等于客户端请求去重。

## 缓存与窗口的边界

- Session 缓存绝对有效期 5 秒，回源前确定截止时间，Redis TTL 不能延长它。撤销先落库，再覆盖 Redis 为撤销标记；标记写入失败时，接口等旧租期结束再报告成功，避免 Redis 恢复后复活旧会话。刷新必须在 MySQL 行锁下验证并轮换哈希。
- 时间线以 ID 排序，热集合完整覆盖最新 1000 条，版本缓存存活 15 秒。发布触发全局版本变化，关注变更只推进该用户版本。Redis 故障期间失效可能丢失，已有旧时间线最迟在 TTL 到期后退出；它不是关注关系的持久化快照。
- 详情有本地 1 秒新鲜缓存。改名/互动后其他 API 实例最多保留这一段正常 L1 延迟；Redis 故障下仍遵循详情 TTL 的最终一致性边界。
- 实时热度桶使用原点赞/评论的时间；撤销互动扣减原分钟而非当前分钟，事件 ID 防止重复增量。榜单生成直接聚合权威记录，避免依赖队列延迟或 Redis 增量完整性。**每个新快照仍需一次 SQL 聚合；已取消全量分钟桶重建与 ZUNIONSTORE 的中间数据搬运。** 无新事件也由定时维护推进窗口。
- 默认快照模式保证翻页顺序不受互动变化影响；可选 `pagination=keyset` 仅解决同分平局，不能保证分数变化时不重复、不遗漏。
- 上传会话最长 24 小时；Redis 会话缓存最多 1 分钟，写操作重新检查 MySQL 状态，已上传分片以磁盘文件为准。

## 启动和诊断

```bash
bash ./start.sh
START_FRONTEND=0 bash ./start.sh
START_WORKER=0 STOP_DOCKER=1 bash ./start.sh
```

Windows 使用 `./start.ps1 -NoFrontend -StopOnExit`，同样支持上述环境变量。脚本只决定本次启动的服务；排除某服务不会停止原本已经运行的该服务。`STOP_DOCKER=1` 停止选定服务，不删除数据卷。

本地进程设置 `PPROF_ENABLED=true` 后可访问 `http://127.0.0.1:6060/debug/pprof/` 或 Worker 的 6061 端口。默认关闭，配置写入 `.env` 后 Compose 会传入容器。容器内部仍只监听自身回环地址，不发布到宿主机；可在容器网络命名空间中使用诊断工具。生产环境没有自动开启该开关。

验证命令与故障注入边界见 [后端验收](backend-validation.md)。

初次容量测试曾发现热榜快照重建超时、Outbox 随评论速率上升出现积压，历史数据见 [首次报告](capacity/2026-09-23/report.md)。后续查询、刷新和投递优化的结果见 [优化复测](capacity/2026-09-25-optimized/report.md)。上述亮点描述实现机制，不构成所有接口已通过容量验收的声明。

首屏刷新采用后台重建：上一快照仍有效且生成时间在两个刷新周期内时可继续服务，超出宽限或游标期限则重新获取快照；`rankedAt` 反映实际生成时间，不伪装成实时排名。
