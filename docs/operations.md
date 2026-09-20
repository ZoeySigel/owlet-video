# 部署与恢复

## 首次准备

目标服务器：Ubuntu 24.04、`owl-et.me`、约 1.6 GiB 内存。新项目独立使用 `owlet-video` 服务账号、`owlet-video-ci` 受限部署账号、`owlet_video` MySQL 数据库/角色。保留旧 Owlet 本地源码与加密备份。

1. 在本地生成**新的**带口令 CI Ed25519 密钥；私钥和口令分别写入新仓库 `production` Environment 的 `DEPLOY_KEY` 与 `DEPLOY_KEY_PASSPHRASE` Secret。工作流通过临时 ssh-agent 解锁。
2. 将公钥和本仓库 `deploy/` 文件传到服务器临时目录，以管理账号运行 `sudo bash deploy/provision.sh REPO_DIR CI_PUBLIC_KEY_FILE`。脚本生成服务器本地数据库、Redis、RabbitMQ、JWT 密钥，保存在 `/etc/owlet-video/app.env`，不复制进 GitHub。
3. 在 GitHub `production` Environment 新增 `DEPLOY_HOST=47.80.1.122` 和固定的 `DEPLOY_KNOWN_HOSTS`。主机记录使用交接文件已验证的 `known_hosts`。
4. 从 Actions 手动运行 **Deploy production**。工作流先执行自动测试，再将版本化产物发送到受限 SSH 入口。入口校验归档、切换 `current` 链接、重启 API/Worker，并在健康检查失败时回滚。
5. 首次部署通过健康检查后，管理账号验证 Caddy 配置并 `systemctl enable --now caddy`，确认 `https://owl-et.me`、手机端、上传、通知和重启自启。

日常部署仅手动点击 Actions，不由 `main` 推送自动上线。旧仓库 Secrets 和旧部署账号不可用于新项目。

## 资源门槛

MySQL、Redis、RabbitMQ、Go API/Worker 和 Caddy 同机运行。MySQL buffer pool 128 MiB、Redis 最大 64 MiB、RabbitMQ 内存水位 256 MiB、Go 服务各 192 MiB；2 GiB swap 仅作故障缓冲。切流前观察内存、swap、OOM 日志、队列积压、磁盘余量及 Feed/上传延迟。若持续换页或 OOM，停止上线并按实测结果申请扩容。上传总量上限为 8 GiB，根分区至少保留 8 GiB 可用空间。

## 备份与恢复

每日 systemd timer 运行 `/usr/local/sbin/owlet-video-backup`：MySQL 一致性 dump 与媒体快照保存到 `/var/backups/owlet-video`，硬链接复用未变化媒体，保留约 7 天。同盘备份不能防止整机丢失；定期安全复制到服务器外，并验证解压与抽样播放。

恢复时先停止 API/Worker 与 Caddy，确认目标为新项目数据库和媒体目录；从选定快照恢复 MySQL dump 与媒体文件，检查数据库记录数、随机媒体校验及权限，再启动服务。版本回滚只切换 `/srv/owlet-video/current` 到上一版本并重启服务；数据库不兼容变更须先执行对应迁移回退方案。

日志：`journalctl -u owlet-video-api -u owlet-video-worker`。RabbitMQ 死信队列在故障修复后人工审查并重投。

## 排名与容错配置

本轮升级增加 `feed_snapshots` 表及点赞、评论时间索引，由 API/Worker 启动时自动迁移。没有删除列或修改业务主键。正式发布前备份数据库；数据量较大时先在预发布环境评估建索引耗时。旧版热门/点赞分页游标会收到 410，刷新首屏即可继续。

API 与 Worker 必须使用相同的排名配置和签名密钥；生产环境可在 `/etc/owlet-video/app.env` 设置下列变量。缺省值适用于当前规模，Compose 从项目 `.env` 读取相同变量。

| 变量 | 默认值 | 用途 |
| --- | --- | --- |
| `HOT_WINDOW` | `24h` | 互动滑动窗口长度 |
| `RANK_REFRESH` | `1m` | 快照时间段及 Worker 刷新间隔 |
| `FEED_SNAPSHOT_TTL` | `15m` | 从快照时间段起点计算的保留时间，应大于刷新间隔 |
| `FEED_RANK_LIMIT` | `2000` | 每个热门/点赞榜单最多条数，允许 20–10000 |
| `REDIS_TIMEOUT` | `200ms` | Redis 单次调用、连接与等待连接池超时 |
| `DB_TIMEOUT` | `3s` | 详情回源、排名等读取任务的超时预算，不是所有业务接口的全局超时 |

非法、非正时长和越界榜单条数回退到默认值。快照 TTL 不大于刷新间隔时会调整为 15 分钟或刷新间隔的两倍，取能满足要求的值。调整窗口与刷新间隔会产生新快照，不修改仍在有效期内的已发出游标。

## 故障观察与恢复

- API：`/livez` 检查进程；`/healthz` 的 MySQL 不可用返回 503，Redis 不可用返回 200 / `degraded`。监控既要读取 HTTP 状态也要读取响应体；进程健康不能代替队列健康。Caddy/Nginx 模板已加入 `/livez` 代理，已有生产 Caddy 配置需随运维发布同步。
- Redis：日志出现 `redis circuit open` 表示进入降级，`redis circuit recovered` 表示恢复。详情响应 `X-Cache: stale` 表示使用旧值。恢复后缓存随请求和 Worker 自动填充，不应通过清空生产 Redis 来执行验收。
- RabbitMQ：日志出现 `broker session unavailable` 时检查连接、队列和权限。Worker 会自动重连；数据库的 `outboxes.published_at IS NULL` 表示仍待发送。积压增加应告警，死信仍需要人工修复与重投，不能把重连等同于死信自动修复。
- 排名：`ranking refresh failed` 表示刷新失败。响应 `X-Feed-Snapshot`、`rankedAt`、`snapshotExpiresAt` 可用于检查版本；`feed_snapshots` 过期行由 Worker 定期删除。如果 Worker 长期停止，API 仍能按需生成榜单，但过期行不会被清理，应恢复 Worker。

缓存失效与 Pub/Sub 是尽力执行，Redis 故障窗口可能造成最多一个缓存 TTL 的计数延迟。登录/注册的降级限流只覆盖单进程预算，不应替代多实例网关的统一限流。数据库/媒体磁盘不可恢复故障仍执行前述备份恢复流程。
