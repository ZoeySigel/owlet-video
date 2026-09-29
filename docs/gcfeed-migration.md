# GCFeed 核心模式与数据迁移

基线：[LeoninCS/GCFeed](https://github.com/LeoninCS/GCFeed/tree/8cf995cfa03985c335594c453ea050df5c0f1b6a/apps/api)，提交 `8cf995cfa03985c335594c453ea050df5c0f1b6a`。核心源码在 `backend/internal`，上游测试在 `backend/test`，来源声明见 `backend/UPSTREAM.md`。

实现边界按用户选择：保留数据并提供迁移，保留本地功能，核心链路对齐 GCFeed。未配置时默认运行旧模式；生产站点已于 2026-09-29 迁移并设置 `BACKEND_ENGINE=gcfeed`，详见[上线验收记录](operations/2026-09-29-gcfeed/report.md)。启动要求目标库存在 verified 标记，不在旧库原地替换表。

## 实现对照

| 链路 | GCFeed 核心 | 兼容和扩展 |
| --- | --- | --- |
| 架构 | domain/application/infra/interfaces | 原 API/Worker 命令装配服务 |
| 推荐 | 128 维 hash n-gram 内容向量、观看兴趣、相似度/热度/新鲜度评分、曝光排除、作者打散 | 推荐入口与旧 JSON 适配 |
| Feed | timeline/hot/following/recommend、缓存、关注回填、推拉混合分发 | 保留点赞快照榜和话题流 |
| 互动 | Redis+RabbitMQ action 与数据库 Worker、评论和关注服务 | 旧路由、@提及、新增收藏及收藏列表 |
| 发布 | video/video_stat，事件驱动向量生成、分发与预热 | 分片/MD5/续传、封面、话题、事务 Outbox |
| 播放 | 曝光、观看事件、配置、预加载、QoS | 原生播放器按实际播放时长上报，详情场景为 watch |
| 账号 | account 表 | 邀请码、密码哈希、Cookie/Session、Refresh Token |
| 消息 | user_message、消息中心、已读 | 旧通知接口、SSE 定期刷新；独立私信表 |
| 指标 | API /metrics、Worker :9091/metrics | 仅向内部监控开放 |

users/videos/likes/comments/follows/notifications 是核心表的兼容视图，避免维护两份数据。会话、邀请码、上传、话题关联、私信及 Outbox 保留实体表。核心模式不得对视图运行旧 AutoMigrate。用户名映射 account 和初始 nickname，视频保留原路径与大小，字段长度覆盖本地数据。上游 internal 路由补齐内部鉴权；应用只挂载所需接口。

推荐采用上游确定性文本向量算法，不调用外部大模型。无历史观看事件时使用冷启动排序，不伪造行为。上游未实现的审核后台、运营后台和系统治理不作为已完成功能。

## 停写与隔离

1. 备份源数据库、完整 DATA_DIR、旧二进制/镜像、配置和 JWT_SECRET。先在备份副本演练。
2. 创建空目标数据库，名称必须不同；准备独立目标目录。目标账号需要建表、建视图和写权限，源账号仅需读取。
3. 停止旧 API 写入；旧 Worker 排空 Outbox、互动命令及 RabbitMQ 未处理/未确认消息后停止。迁移程序只检查数据库待办，不能替代队列排空检查。
4. 使用独立 Redis 实例或逻辑 DB，以及独立 RabbitMQ 实例/vhost。扩展仍用旧队列名，仅靠 owlet.core 前缀不能隔离。
5. 迁移进程与原/目标运行时需使用相同操作系统路径语义。Linux 容器记录的上传路径应由 Linux 迁移程序处理，不能直接交给 Windows 程序。

## 迁移工具

在 backend 目录运行 `go build -o owlet-video .`，设置环境变量（PowerShell 使用 `$env:NAME='value'`，Linux 使用 `export NAME='value'`）：

| 变量 | 含义 |
| --- | --- |
| SOURCE_MYSQL_DSN | 源 DSN，包含 parseTime=true&loc=UTC |
| TARGET_MYSQL_DSN | 空目标 DSN，包含 parseTime=true&loc=UTC |
| SOURCE_DATA_DIR | 迁移进程读取的完整源目录 |
| TARGET_DATA_DIR | 独立目标目录，不能相同或互为父子目录 |
| SOURCE_RUNTIME_DATA_DIR | 源服务 DATA_DIR，默认 SOURCE_DATA_DIR |
| TARGET_RUNTIME_DATA_DIR | 目标服务 DATA_DIR，默认 TARGET_DATA_DIR |
| MIGRATION_SOURCE_QUIESCED | 停写并排空后设为 1 |

```sh
./owlet-video migrate-core inspect
./owlet-video migrate-core apply
```

inspect 检查待办、媒体存在/大小并输出数量，不写入目标。apply 先检查目标状态，复制整个数据目录，包括头像、封面和上传分片，以 SHA-256 核验每个文件。同名不同内容、额外目标文件、符号链接会阻断，不覆盖或删除。数据库事务保留主键、密码哈希、时间、会话、通知和私信；上传 StoredPath 转为目标运行时路径。最后核验数量、关键字段摘要、核心引用和互动计数，回填向量，设置 verified。

prepared 状态可重试复制事务，copied 可重试核验/向量回填，verified 拒绝重放。不要手工改标记。文件复制若强制中断留下不完整目标文件，重试会因校验不一致中止；保留故障目录，改用新的空目标目录重做。源应全程停写，不支持在线增量迁移。

## 独立本地启动

compose.gcfeed.yml 单独使用，不与旧文件通过 -f 合并。它继承基础服务配置，但采用独立项目卷、数据库 owlet_gcfeed、data-core 目录、页面端口 3001；需要支持 !override 的 Docker Compose。设置与旧环境相同的 JWT_SECRET，以保留会话。默认密码仅用于本地。

```sh
docker compose -f compose.gcfeed.yml up -d --wait mysql redis rabbitmq
```

目标 MySQL 为宿主机 127.0.0.1:33306，默认用户 owlet_core，密码来自 MYSQL_PASSWORD（默认 local-app-password）。先执行上述迁移：目标目录为仓库 data-core，容器目标运行时目录为 /var/lib/owlet-video。用 Linux 迁移容器时将源目录只读挂载、目标读写挂载，确认目标文件权限允许应用用户读写。

收到 state: verified 后：

```sh
docker compose -f compose.gcfeed.yml up -d --build api worker web
```

浏览 http://localhost:3001，确认 /api/v1/capabilities 返回 engine: gcfeed。验证旧账号登录/刷新、旧视频播放、续传发布、推荐、收藏、关注、评论、通知和私信。旧分页游标不可复用。自定义部署同时更新 API/Worker/Web 的路径和配置；现有生产部署脚本不会自动切换。

## 回滚

切换前源库和源文件不变，可停止目标服务恢复旧入口。切换后如产生新增账号、互动、上传或私信，先停写并备份目标，再处理新增数据；不能直接切回源库而声称保留新数据。当前没有反向增量迁移。两个版本不能同时接受同一业务流量的写入。

## 验证与限制

后端 go test ./... / go vet ./...；前端 typecheck、test:navigation、test:upload、build。真实依赖使用 compose.test.yml。CORE_TEST_MYSQL_DSN 需允许创建临时数据库；另设 CORE_TEST_REDIS_ADDR、CORE_TEST_RABBITMQ_URL。仅使用隔离测试实例：测试创建/删除随机数据库、清空 Redis DB 14。CI 已配置这些变量。

TestCoreMigrationAndCompatibility 覆盖字段摘要、源只读、拒绝重放、媒体复制、旧密码登录与历史 Refresh Token、曝光排除、关注/评论、收藏 MQ 落库及列表、通知已读、私信、邀请码注册，以及迁移中待发布上传→发布→Outbox→MQ→向量生成。OlderSchema 用例覆盖缺少历史可选表的源库；媒体单测覆盖重试、目录重叠、内容冲突、路径越界。生产迁移已完成，数量、媒体和备份恢复结果见上述上线验收记录。

核心 action 的异步一致性和故障恢复语义与原本地事务命令不同，不能直接沿用旧容量结论。核心消息 SSE 依赖定期刷新。本次已在隔离环境完成消息代理断线重启恢复、并发读取和浏览器播放/收藏验收；这些结果不代表生产容量保证。

RabbitMQ 发布增加 publisher confirms（5 秒确认超时）；连接失效后进程退出，由 systemd/容器重启。观看事件同步写入数据库供推荐读取，没有启用上游可选的观看事件 MQ 发布，因为该基线没有对应画像消费者，启用会形成无人消费的积压。指标仅绑定回环地址。Docker Web 代理使用运行时 DNS 解析，避免 API 容器重启换 IP 后持续 502。
