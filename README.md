# Owlet Video

[![Test](https://github.com/ZoeySigel/owlet-video/actions/workflows/ci.yml/badge.svg)](https://github.com/ZoeySigel/owlet-video/actions/workflows/ci.yml)

Owlet Video 是一个前后端分离的视频分享应用，支持视频上传与播放、内容浏览、用户关注、点赞评论、私信和站内通知。后端使用 Go 提供 REST API 和异步任务处理，前端使用 Next.js 构建并静态部署。

界面采用银白与电光蓝的 Y2K / Retro Internet 风格，提供桌面端视频网格、独立播放页及移动端导航。

[在线访问](https://owl-et.me) · [架构文档](docs/architecture.md) · [部署与恢复](docs/operations.md)

## 功能

| 模块 | 功能说明 |
| --- | --- |
| 内容浏览 | 按最新、热度、点赞数浏览视频，支持关注动态、话题列表和游标分页 |
| 视频发布 | MP4 分片上传、MD5 校验、断点续传、可选封面及话题标签 |
| 视频播放 | 独立详情页、原生播放控制、横竖视频适配、链接分享 |
| 用户互动 | 点赞、评论、删除本人评论、关注用户及查看喜欢的视频 |
| 用户主页 | 作品列表、个人简介、粉丝、关注和获赞统计 |
| 私信与通知 | 用户私信、通知列表、未读计数、已读管理及 SSE 通知更新 |
| 账号管理 | 邀请码注册、登录与会话刷新、头像与资料编辑、密码修改 |

访客可浏览公开视频、用户主页和评论。发布、互动和私信等操作需要登录；注册使用一次性邀请码。

登录与注册按 IP 分别限流：每小时最多 60 次登录请求、20 次注册请求。成功和失败请求均计数。互动按账号限流：点赞/取消每分钟 30 次、评论/删除 10 次、关注/取关 20 次。Redis 故障时保留本地剩余预算。

## 技术栈

| 层级 | 技术 | 用途 |
| --- | --- | --- |
| 前端 | Next.js 16、React 19、TypeScript | 页面与交互、静态导出 |
| API | Go 1.24、Gin、GORM | HTTP 接口、身份认证、业务事务 |
| 数据库 | MySQL 8.4 | 用户、视频、互动、消息及事件持久化 |
| 缓存 | Redis 7 | 视频详情缓存、请求限流、通知 Pub/Sub 和 Feed 索引维护 |
| 消息队列 | RabbitMQ 4 | 异步事件分发、失败重试和死信处理 |
| 运行与部署 | Docker Compose、Nginx、Caddy、systemd | 本地环境编排、静态文件托管、反向代理与进程管理 |
| 持续集成 | GitHub Actions | 类型检查、构建、后端测试和手动生产部署 |

## 架构概览

应用由静态前端、API 和 Worker 三部分组成。API 与 Worker 使用同一份 Go 代码，通过启动参数选择运行模式。

- **请求链路**：浏览器通过同源反向代理访问 `/api/v1`，视频等媒体文件由 Web 服务器直接提供。
- **业务数据**：MySQL 保存业务数据与不可变榜单快照。最新与关注使用 Redis 热时间线、MySQL 冷数据拼接及 ID 游标；热门和最多点赞使用签名快照游标，优先读取 Redis 有序集合，缓存故障时读取持久化快照。
- **异步事件**：互动命令与 Outbox 同事务提交，Worker 消费后写入业务数据和通知事件；MQ 故障支持幂等直写。默认保持同步响应结构，也支持 `Prefer: respond-async` 返回 202。
- **通知更新**：通知先持久化，再通过 Redis Pub/Sub 和 SSE 提醒前端刷新。连接中断后仍可读取历史通知。
- **文件存储**：视频分片写入临时目录，校验并完成发布后移入媒体目录。备份与恢复需要同时覆盖数据库和媒体文件。

表结构、缓存策略及消息一致性设计见 [架构文档](docs/architecture.md)。参考项目的 21 项后端亮点、接口用法和实现边界见 [后端亮点对照](docs/backend-highlights.md)。

## 后端设计

| 能力 | 实现与边界 |
| --- | --- |
| 稳定排序分页 | 热门与点赞榜固定快照，按分数、视频 ID 排序；互动计数变化不会移动已发出游标中的视频位置。默认快照保留 15 分钟、最多 2000 条，过期后提示重新加载 |
| 滑动窗口热榜 | 最近 24 小时有效点赞 × 3 + 评论 × 5；SQL 内聚合并截取前 K 条，Redis 缓存与持久化快照保证稳定翻页 |
| 事件驱动异步处理 | 持久化互动命令、事务性 Outbox、发布确认、幂等直写、延迟重试和死信；Worker 断线后退避重连 |
| 热点缓存保护 | 详情请求合并、带令牌的 Redis 重建锁、原子回填、空值缓存、TTL 抖动与回源并发上限 |
| 降级与恢复 | Redis 超时与熔断、数据库回退、短期旧详情兜底、本地限流、榜单缓存自动重建、SSE 定期刷新通知 |
| 工程交付 | 独立测试容器、真实依赖故障注入、Go 竞争检测、前端类型检查与静态构建、手动发布及健康检查回滚 |

这些机制提供应用层的故障降级与恢复，当前单机部署不具备数据库、消息队列或主机级故障转移。测试方法与限制见 [后端验收](docs/backend-validation.md)。

本地容量实测、失败档位与复现方法见 [容量压测](docs/capacity/README.md)。初次压测发现热榜重建和 Outbox 投递瓶颈，已针对查询结果规模与批量投递优化；实际容量以对应版本的复测报告为准。

## 快速启动

### 环境要求

- Git
- Docker Engine 或 Docker Desktop
- Docker Compose v2

使用 Docker Compose 启动时，无需在宿主机单独安装 Go、Node.js、MySQL、Redis 或 RabbitMQ。

### 1. 获取代码并配置环境

```bash
git clone https://github.com/ZoeySigel/owlet-video.git
cd owlet-video
cp .env.example .env
```

Windows PowerShell 中使用 `Copy-Item .env.example .env` 复制配置文件。

编辑 `.env`，设置以下变量：

| 变量 | 说明 |
| --- | --- |
| `MYSQL_ROOT_PASSWORD` | MySQL 管理员密码 |
| `MYSQL_PASSWORD` | 应用数据库用户密码 |
| `REDIS_PASSWORD` | Redis 访问密码 |
| `RABBITMQ_USER` | RabbitMQ 用户名 |
| `RABBITMQ_PASSWORD` | RabbitMQ 密码 |
| `JWT_SECRET` | Token 签名密钥，至少 32 字符 |

示例值仅用于本地开发。对外部署前应更换密码和签名密钥，且不要将 `.env` 或部署私钥提交到仓库。

### 2. 启动服务

```bash
docker compose up -d --build
docker compose ps
```

首次启动会拉取依赖镜像、构建应用并初始化数据库表。服务就绪后访问 [http://localhost:3000](http://localhost:3000)。

也可运行 `bash ./start.sh` 或 Windows 下的 `./start.ps1`。脚本支持前端/Worker 开关；诊断与开关说明见 [后端亮点对照](docs/backend-highlights.md)。

### 3. 创建账号

```bash
docker compose exec api /usr/local/bin/owlet-video invite
```

命令会输出一个有效期为 14 天的一次性邀请码。在网站登录页选择“创建账号”，填写邀请码完成注册。可重复运行命令为其他用户生成邀请码。

### 生成本地测试数据

启动 Docker Compose 后，可一次生成 6 个随机账号、24 条可播放的短视频，以及用于测试排序和互动的点赞、评论、关注数据：

```bash
docker compose run --rm -e SEED_TEST_DATA=1 api seed
```

命令会在终端输出本次创建的账号和密码，请立即保存。重复执行会新增一批数据，不会覆盖现有账号或视频。可用 `-users` 和 `-videos` 调整数量，例如 `docker compose run --rm -e SEED_TEST_DATA=1 api seed -users 4 -videos 12`。该命令只在显式设置 `SEED_TEST_DATA=1` 时执行；请仅用于本地或专用测试环境。

### 常用操作

```bash
# 查看 API 和 Worker 日志
docker compose logs -f api worker

# 检查服务健康状态
curl http://localhost:3000/healthz

# 修改前端后重新构建 Web 服务
docker compose up -d --build web

# 停止服务，保留持久化数据
docker compose down
```

MySQL、Redis 和 RabbitMQ 数据保存在 Docker 命名卷中；视频与上传临时文件保存在项目的 `data/` 目录。RabbitMQ 管理界面为 [http://localhost:15672](http://localhost:15672)，使用 `.env` 中配置的账号登录。

## 项目结构

```text
owlet-video/
├── backend/               # Go API、Worker、数据模型与测试
├── frontend/
│   ├── app/               # Next.js 入口、页面元数据与全局样式
│   └── src/               # 页面交互、共享组件、API 客户端与上传逻辑
├── deploy/                # 服务器初始化、发布、备份与服务配置
├── docs/                  # 架构、运维与前端验收文档
├── .github/workflows/     # 自动测试与手动部署工作流
├── .env.example           # 本地环境变量示例
└── docker-compose.yml     # 本地服务编排
```

## 开发与测试

宿主机执行检查需要 Go 1.24 及以上版本、Node.js 22 和 npm。带 `-race` 的 Go 测试还需要当前平台支持的 C 工具链。

```bash
# 后端静态检查与测试
cd backend
go vet ./...
go test -race -count=1 ./...

# 前端依赖安装、类型检查与生产构建
cd ../frontend
npm ci
npm run typecheck
npm run build
```

前端构建产物位于 `frontend/out/`。前端以同源方式访问 API，单独运行 `npm run dev` 不会自动连接 Docker 中的后端；完整功能验证可使用上述 Compose 环境。

集成测试使用 `TEST_MYSQL_DSN`、`TEST_REDIS_ADDR`、`TEST_REDIS_PASSWORD` 和 `TEST_RABBITMQ_URL` 配置测试依赖。未设置 `TEST_MYSQL_DSN` 时跳过集成测试；RabbitMQ 相关验证需要配置 `TEST_RABBITMQ_URL`。请使用独立测试数据库与消息队列。

仓库提供 `compose.test.yml`，使用独立项目名、端口和临时数据库，不占用开发环境的数据。具体命令见 [后端验收](docs/backend-validation.md)。

GitHub Actions 在 `main` 推送和 Pull Request 时运行检查，并启动 MySQL、Redis、RabbitMQ 执行集成测试。

## API 入口

业务接口统一使用 `/api/v1` 前缀。`GET /livez` 检查 API 进程存活；`GET /healthz` 检查数据库与缓存，Redis 故障时返回 HTTP 200 和 `status: degraded`，数据库故障时返回 HTTP 503。

| 模块 | 主要接口 |
| --- | --- |
| 账号 | `POST /auth/register`、`POST /auth/login`、`POST /auth/refresh`、`POST /auth/logout`、`GET /auth/me` |
| 个人资料 | `PATCH /me`、`POST /me/avatar`、`PATCH /auth/password` |
| 用户 | `GET /users/:id`、`GET /users/:id/videos`、`GET /users/:id/followers`、`GET /users/:id/following` |
| 内容 | `GET /videos`、`GET /videos/:id`、`GET /tags/:tag/videos`、`POST /videos` |
| 上传 | `POST /uploads`、`GET /uploads/:id`、`PUT /uploads/:id/chunks/:index`、`POST /uploads/:id/complete`、`POST /covers` |
| 互动 | `GET/PUT/DELETE /videos/:id/like`、`GET/POST /videos/:id/comments`、`DELETE /comments/:id`、`PUT/DELETE /users/:id/follow`、`GET /me/likes` |
| 异步操作 | `GET /interactions/:id`（查询当前用户的异步互动命令结果） |
| 私信 | `GET /messages`、`GET/POST /messages/:peer` |
| 通知 | `GET /notifications`、`GET /notifications/unread`、`PATCH /notifications/read`、`GET /notifications/stream` |

接口注册及鉴权范围以 [backend/main.go](backend/main.go) 为准。

## 部署

本地 Compose 环境使用 Nginx 托管前端。生产环境使用 Caddy 提供 HTTPS、静态文件与 API 反向代理，API 和 Worker 由 systemd 管理。

生产发布通过 GitHub Actions 的 **Deploy production** 工作流手动触发：先运行测试，再构建 Linux 二进制和静态前端，将产物传送至受限 SSH 部署入口。部署程序按提交版本保存产物、切换当前版本并执行健康检查；检查失败时回滚至上一版本。

`main` 分支推送不会自动触发生产部署。服务器准备、部署凭据、备份及恢复步骤见 [部署与恢复](docs/operations.md)。

## 当前范围

- 单个视频上限为 200 MiB，分片大小为 5 MiB；媒体存储在服务器本地文件系统。
- 播放使用上传的原始 MP4 文件，未提供自动转码、多码率或自适应流媒体；能否播放取决于浏览器对文件编码的支持。
- 内容发现采用时间、热度、点赞数与关注关系排序，未接入个性化推荐算法。
- 前端采用静态导出与查询参数路由，视频分享地址为 `/?view=watch&id=<视频 ID>`。

## 参考

项目功能范围参考 [feedsystem_video_go](https://github.com/LeoninCS/feedsystem_video_go)，当前 Go 后端与 Next.js 前端为独立实现。
