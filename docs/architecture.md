# GCFeed 后端架构

GCFeed 是唯一运行后端，启动不再读取 BACKEND_ENGINE。核心源码在 backend/internal，来源与基线见 [UPSTREAM](../backend/UPSTREAM.md)。

## 数据与适配

核心实体表包括 account、video、video_stat、interaction_action、interaction_comment、user_follow、user_message、video_view_event、video_embedding。users、videos、likes、comments、follows、notifications 为兼容视图，供 Owlet 既有前端契约读取。注册和发布直接写核心实体，会话、邀请码、分片上传、话题、私信与发布 Outbox 使用扩展表。

兼容视图不可运行旧实体的 AutoMigrate。init-core 只初始化空库，对已验证 GCFeed 库直接返回，拒绝旧库和未完成的迁移。migrate-core 保留单向旧数据导入能力，源模型仅用于读取和测试，不提供旧后端运行入口。

## 请求与异步处理

API 和 Worker 共用 Go 二进制。GCFeed 的 domain/application/infra/interfaces 分层处理 Feed、推荐、互动、关系、消息、曝光、播放与向量。根目录保留 Owlet 的 Cookie/Session、刷新令牌、邀请码、分片上传、头像、话题、私信以及 JSON 适配。

时间线、热门、关注和推荐由 GCFeed Feed 服务处理。推荐使用 128 维 hash n-gram 内容向量、观看兴趣、相似度、热度及新鲜度，排除近期曝光并打散作者。

点赞和收藏通过 Redis 与 RabbitMQ 异步处理，评论和关注调用核心服务。视频发布与 Outbox 同事务提交，扩展 Worker 确认消息后转发核心发布事件，核心 Worker 处理关注分发、预热与向量生成。扩展 Outbox 不再执行旧互动命令、旧通知生成或旧榜单维护。

详情缓存保留请求合并、重建锁、空值缓存及短期旧值兜底。消息中心由 GCFeed 持久化；既有通知接口和 SSE 继续提供前端适配，私信使用独立表。

## 部署与检查

默认 Compose 使用 owlet-gcfeed 项目、owlet_gcfeed 数据库及 data-core 媒体目录。core-init 等待依赖与目录权限准备完成，API/Worker 等待初始化成功。旧容量压测程序与旧榜单配置已删除，历史报告不代表 GCFeed 容量。

GET /livez 检查进程；GET /healthz 检查数据库和缓存，Redis 不可用时返回 degraded，数据库不可用返回 503。GET /api/v1/capabilities 固定返回 engine: gcfeed。指标由 /metrics 和 Worker 127.0.0.1:9091/metrics 提供。服务为单机部署。
