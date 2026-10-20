# Owlet Video

[![Test](https://github.com/ZoeySigel/owlet-video/actions/workflows/ci.yml/badge.svg)](https://github.com/ZoeySigel/owlet-video/actions/workflows/ci.yml)

Owlet Video 是一个前后端分离的视频分享应用，支持视频上传与播放、内容浏览、用户关注、点赞评论、私信和站内通知。后端使用 Go 提供 REST API 和异步任务处理，前端使用 Next.js 构建并静态部署。

界面采用银白与电光蓝的 Y2K / Retro Internet 风格，提供桌面端视频网格、独立播放页及移动端导航。

[在线访问](https://owl-et.me) · [架构文档](docs/architecture.md) · [部署与恢复](docs/operations.md)

GCFeed 是唯一后端，提供推荐、曝光去重、内容向量、关注分发、异步点赞/收藏、消息中心和播放 QoS，并保留邀请码、刷新会话、分片续传、话题及私信。已删除旧后端运行分支，不再需要 `BACKEND_ENGINE`。已有旧数据请按 [迁移说明](docs/gcfeed-migration.md) 导入独立目标库；现有 GCFeed 数据可以直接继续使用。
## 功能

管理员后台已提供账号搜索、资料编辑、密码重置、停用恢复，以及视频预览、编辑、下架恢复。入口为 `/?view=admin`；首次授权和使用说明见 [管理后台](docs/admin.md)。

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

GCFeed 核心代码位于 `backend/internal/`，按 domain、application、infra、interfaces 分层；`backend/` 根目录负责运行装配及 Owlet 的会话、上传、话题、私信和前端 API 适配。

- MySQL 核心表保存账号、视频、互动、关注、消息、观看事件与内容向量；兼容视图支持现有前端读接口。
- Redis 缓存 Feed、详情与会话，维护异步互动状态；RabbitMQ Worker 处理互动、关注分发及视频向量生成。
- 发布视频与扩展 Outbox 同事务提交，Worker 可靠转发 GCFeed 发布事件。
- 推荐使用确定性文本向量、观看兴趣、热度和新鲜度，并排除近期曝光和打散作者。
- 最新、热门、关注、推荐使用 GCFeed 分页；最多点赞通过核心统计视图按点赞数与 ID 游标翻页，计数变化会影响后续页面。
- 视频和上传分片保存在本地目录；数据库和媒体需要一起备份。

详见 [架构文档](docs/architecture.md)、[后端验证](docs/backend-validation.md) 和 [上游来源](backend/UPSTREAM.md)。历史旧后端容量报告仅作为归档。

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

首次启动会拉取依赖镜像、构建应用并初始化 GCFeed 数据库与兼容视图。服务就绪后访问 [http://localhost:3000](http://localhost:3000)。

也可运行 `bash ./start.sh` 或 Windows 下的 `./start.ps1`。脚本支持前端/Worker 开关；诊断与开关说明见 [后端亮点对照](docs/backend-highlights.md)。

### 3. 创建账号

```bash
docker compose exec api /usr/local/bin/owlet-video invite
```

命令会输出一个有效期为 14 天的一次性邀请码。在网站登录页选择“创建账号”，填写邀请码完成注册。可重复运行命令为其他用户生成邀请码。

### 数据初始化

Compose 的 `core-init` 服务会初始化空库，并在成功后启动 API 与 Worker。已验证的 GCFeed 数据库保持不变；遇到旧库或未完成迁移的库会拒绝启动，应使用迁移工具。旧版 `seed` 命令已移除。

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

MySQL、Redis 和 RabbitMQ 数据保存在 Docker 命名卷中；视频与上传临时文件保存在项目的 `data-core/` 目录。RabbitMQ 管理界面为 [http://localhost:15672](http://localhost:15672)，使用 `.env` 中配置的账号登录。

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
| GCFeed | `GET /capabilities`、`GET /feed-items`、`POST /feed-queries`、`PUT/DELETE /videos/:id/favorite`、`GET /me/favorites`、`GET /message-center`、`POST /video-view-events`、`POST /playback-qos-reports` |
| 私信 | `GET /messages`、`GET/POST /messages/:peer` |
| 通知 | `GET /notifications`、`GET /notifications/unread`、`PATCH /notifications/read`、`GET /notifications/stream` |

接口注册及鉴权范围以 [backend/main.go](backend/main.go) 和 [backend/core.go](backend/core.go) 为准。

## 部署

本地 Compose 环境使用 Nginx 托管前端。生产环境使用 Caddy 提供 HTTPS、静态文件与 API 反向代理，API 和 Worker 由 systemd 管理。

生产发布通过 GitHub Actions 的 **Deploy production** 工作流手动触发：先运行测试，再构建 Linux 二进制和静态前端，将产物传送至受限 SSH 部署入口。部署程序按提交版本保存产物、切换当前版本并执行健康检查；检查失败时回滚至上一版本。

`main` 分支推送不会自动触发生产部署。服务器准备、部署凭据、备份及恢复步骤见 [部署与恢复](docs/operations.md)。

## 当前范围

- 单个视频默认上限为 8 GiB，分片大小为 5 MiB；媒体存储在服务器本地文件系统。
- 播放使用上传的原始 MP4 文件，未提供自动转码、多码率或自适应流媒体；能否播放取决于浏览器对文件编码的支持。
- 内容发现支持最新、热门、点赞、关注和基于观看兴趣与内容向量的推荐。
- 前端采用静态导出与查询参数路由，视频分享地址为 `/?view=watch&id=<视频 ID>`。

## 参考

项目功能范围参考 [feedsystem_video_go](https://github.com/LeoninCS/feedsystem_video_go)，当前 Go 后端与 Next.js 前端为独立实现。
