# Owlet Video

[![Test](https://github.com/ZoeySigel/owlet-video/actions/workflows/ci.yml/badge.svg)](https://github.com/ZoeySigel/owlet-video/actions/workflows/ci.yml)

Owlet Video 是一个前后端分离的视频社区，提供视频发布与播放、个性化推荐、关注动态、点赞收藏、评论、私信通知和管理员后台。前端采用 Next.js 与 React，后端使用 Go，配合 MySQL、Redis 和 RabbitMQ 完成内容存储、缓存与异步任务处理。

界面采用银白与电光蓝的 Y2K / Retro Internet 风格，支持桌面端视频网格、独立播放页和移动端导航。

[在线体验](https://owl-et.me) · [快速启动](#快速启动) · [开发与测试](#开发与测试) · [管理后台](docs/admin.md)

## 功能

| 模块 | 能力 |
| --- | --- |
| 内容发现 | 为你推荐、时间线、热门、关注四种 Feed，支持话题浏览和游标分页 |
| 视频发布 | MP4 分片上传、断点续传、MD5 校验、自定义封面和话题标签 |
| 视频播放 | 独立播放页、横竖视频适配、链接分享、观看行为与播放质量上报 |
| 社区互动 | 点赞、收藏、评论、删除本人评论、关注用户及查看喜欢与收藏的视频 |
| 用户主页 | 作品列表、个人简介、粉丝、关注与获赞统计 |
| 消息通知 | 用户私信、互动通知、未读计数、批量已读和 SSE 通知更新 |
| 账号管理 | 一次性邀请码注册、登录与会话刷新、头像与资料编辑、密码修改 |
| 管理后台 | 站点概况、账号与视频搜索、资料编辑、密码重置、账号停用恢复、视频下架恢复 |

访客可以浏览公开视频、用户主页和评论。推荐、关注动态、发布、互动及私信需要登录，管理后台仅向管理员开放。

## 实现特点

- **结合内容与行为的推荐**：使用标题与简介的 Hash-Ngram 内容向量，结合观看兴趣、热度和新鲜度排序，同时排除近期曝光并打散作者。
- **可恢复的视频上传**：以 5 MiB 分片上传文件，记录上传进度并校验 MD5，支持中断后继续上传。
- **异步处理与可靠发布**：API 和 Worker 分别处理在线请求与后台任务；视频发布通过事务 Outbox 投递事件，RabbitMQ 承载互动处理、关注分发和内容向量生成。
- **缓存与依赖保护**：视频详情使用请求合并、重建锁和短期旧值兜底；登录、注册及互动入口提供限流，依赖调用设置超时。
- **账号与内容管理**：管理操作检查账号角色和状态；改名、重置密码或停用账号会撤销其会话，视频下架保留媒体和互动记录，便于恢复。

## 技术栈与架构

| 层级 | 技术 |
| --- | --- |
| 前端 | Next.js 16、React 19、TypeScript，静态导出 |
| 后端 | Go 1.24、Gin、GORM，REST API 与独立 Worker |
| 数据存储 | MySQL 8.4，保存账号、视频、互动、消息与观看事件 |
| 缓存 | Redis 7，用于 Feed、视频详情、互动状态与通知分发 |
| 消息队列 | RabbitMQ 4，用于异步任务与事件消费 |
| 部署 | Docker Compose、Nginx、Caddy、systemd |
| 持续集成 | GitHub Actions，类型检查、测试、构建与手动生产发布 |

前端与 API 通过同一域名提供服务。Nginx 或 Caddy 托管静态页面和媒体文件，并将 `/api/` 请求转发至 Go API。API 访问 MySQL、Redis 和 RabbitMQ，Worker 消费事件并执行后台任务。

后端业务按 `domain`、`application`、`infra` 和 `interfaces` 分层；服务入口负责运行装配、会话认证、上传、话题、私信和管理接口。API 与 Worker 使用同一个 Go 二进制，通过启动参数选择运行方式。

视频、封面、头像及上传临时文件存储在本地文件系统，数据库与媒体文件共同构成站点的持久化数据。

## 快速启动

### 1. 准备环境

安装 Git、Docker Engine 或 Docker Desktop，以及 Docker Compose v2。使用容器启动时，无需在宿主机安装 Go、Node.js 或数据库。

```bash
git clone https://github.com/ZoeySigel/owlet-video.git
cd owlet-video
cp .env.example .env
```

Windows PowerShell 使用 `Copy-Item .env.example .env` 复制配置文件。

在 `.env` 中配置以下变量。示例值用于本地开发，对外部署时应设置独立密码与随机签名密钥。

| 变量 | 用途 |
| --- | --- |
| `MYSQL_ROOT_PASSWORD` | MySQL 管理员密码 |
| `MYSQL_PASSWORD` | 应用数据库用户密码 |
| `REDIS_PASSWORD` | Redis 访问密码 |
| `RABBITMQ_USER` / `RABBITMQ_PASSWORD` | RabbitMQ 账号与密码 |
| `JWT_SECRET` | Token 签名密钥，至少 32 字符 |

### 2. 启动服务

```bash
docker compose up -d --build
docker compose ps
```

Compose 会启动 MySQL、Redis、RabbitMQ，完成数据库初始化与存储目录准备后运行 API、Worker 和 Web。服务就绪后访问 [http://localhost:3000](http://localhost:3000)。

数据库与消息队列数据保存在 Docker 命名卷中，媒体及上传临时文件位于 `data-core/`。RabbitMQ 管理界面位于 [http://localhost:15672](http://localhost:15672)，使用 `.env` 中的 RabbitMQ 账号登录。

### 3. 注册账号

```bash
docker compose exec api /usr/local/bin/owlet-video invite
```

命令会生成有效期为 14 天的一次性邀请码。在网站登录页选择“创建账号”，填写邀请码完成注册。为其他用户开放注册时，可以再次生成邀请码。

### 4. 配置管理员

为一个已注册账号授予管理员权限，将 `YOUR_USERNAME` 替换为实际用户名：

```bash
docker compose exec api /usr/local/bin/owlet-video grant-admin YOUR_USERNAME
```

刷新页面后，从管理入口或 `/?view=admin` 进入后台。操作范围与权限说明见 [管理后台](docs/admin.md)。

### 常用操作

```bash
# 查看 API 和 Worker 日志
docker compose logs -f api worker

# 检查数据库与缓存健康状态
curl http://localhost:3000/healthz

# 修改前端后重新构建 Web
docker compose up -d --build web

# 停止服务并保留持久化数据
docker compose down
```

## 项目结构

```text
owlet-video/
├── backend/
│   ├── internal/
│   │   ├── domain/           # 领域模型与接口
│   │   ├── application/      # Feed、推荐、互动、关系与消息服务
│   │   ├── infra/            # 数据库、缓存、消息队列等实现
│   │   └── interfaces/       # 业务接口层
│   ├── test/                 # 业务测试
│   └── main.go               # API、Worker 与管理命令入口
├── frontend/
│   ├── app/                  # Next.js 入口、元数据与全局样式
│   ├── src/                  # 页面、组件、API 客户端与上传逻辑
│   └── scripts/              # 上传与导航测试
├── deploy/                   # 发布、备份、巡检与服务配置
├── docs/                     # 设计、运维与使用文档
├── .github/workflows/        # CI 与生产发布工作流
├── .env.example              # 本地配置示例
├── compose.test.yml          # 独立测试依赖
└── docker-compose.yml        # 本地完整服务编排
```

## 开发与测试

在宿主机开发需要 Go 1.24 及以上版本、Node.js 22 和 npm。运行 Go 的竞态检测还需要当前平台支持的 C 工具链。

```bash
# 后端
cd backend
go vet ./...
go test -race -count=1 ./...

# 前端
cd ../frontend
npm ci
npm run typecheck
npm run test:upload
npm run test:navigation
npm run build
```

前端开发服务器通过 `npm run dev` 启动，生产构建输出到 `frontend/out/`。API 使用同源路径，单独启动前端时需配置反向代理；完整功能验证可直接使用 Compose 环境。

集成测试通过 `TEST_MYSQL_DSN`、`TEST_REDIS_ADDR`、`TEST_REDIS_PASSWORD`、`TEST_RABBITMQ_URL` 及 `CORE_TEST_MYSQL_DSN`、`CORE_TEST_REDIS_ADDR`、`CORE_TEST_RABBITMQ_URL` 配置依赖。未配置相应依赖时，部分测试会跳过。请使用专用测试数据库与消息队列；`compose.test.yml` 提供独立端口的测试依赖，CI 会启动 MySQL、Redis 和 RabbitMQ 执行集成测试。

## API 概览

业务接口使用 `/api/v1` 前缀，以下路径均相对于此前缀：

| 模块 | 代表接口 |
| --- | --- |
| 账号与会话 | `POST /auth/register`、`POST /auth/login`、`POST /auth/refresh`、`POST /auth/logout`、`GET /auth/me` |
| 资料与主页 | `PATCH /me`、`POST /me/avatar`、`GET /users/:id`、`GET /users/:id/videos` |
| 内容发现 | `GET /videos`、`GET /feed-items`、`POST /feed-queries`、`GET /tags/:tag/videos` |
| 上传与发布 | `POST /uploads`、`PUT /uploads/:id/chunks/:index`、`POST /uploads/:id/complete`、`POST /videos` |
| 互动与关系 | `PUT/DELETE /videos/:id/like`、`GET/POST /videos/:id/comments`、`PUT/DELETE /users/:id/follow` |
| 消息与通知 | `GET/POST /messages/:peer`、`GET /notifications`、`GET /notifications/stream`、`GET /message-center` |
| 观看与播放 | `POST /video-view-events`、`POST /playback-qos-reports` |
| 管理后台 | `GET /admin/overview`、`GET /admin/users`、`GET /admin/videos` |

完整路由与鉴权规则见 [main.go](backend/main.go)、[core.go](backend/core.go) 和 [admin.go](backend/admin.go)。

`GET /livez` 检查 API 进程存活，`GET /healthz` 检查数据库与缓存状态。数据库不可用时返回 HTTP 503；Redis 不可用时返回 HTTP 200，并在响应体中标记 `status: degraded`。

## 部署与运行

本地环境使用 Docker Compose 和 Nginx。生产环境使用 Caddy 提供 HTTPS、静态文件托管和 API 反向代理，由 systemd 管理 API 与 Worker。

GitHub Actions 的 **Deploy production** 工作流通过手动触发发布：先运行测试，再构建 Linux 二进制与静态前端，将发布包发送至受限 SSH 入口。部署程序按提交版本保存产物，切换当前版本并检查健康状态，检查失败时回滚。推送 `main` 会触发 CI，生产部署需单独触发。

部署相关文件位于 `deploy/`，包括服务器准备脚本、Caddy 配置、systemd 服务、发布程序、备份脚本和定时巡检。运行时配置与凭据由部署环境管理；备份应同时覆盖数据库与媒体文件，并定期验证恢复结果。

## 使用边界

- 默认单个视频上限为 8 GiB，可通过后端 `MAX_MEDIA_BYTES` 调整；媒体存储使用本地文件系统。
- 播放使用原始 MP4，未提供自动转码或多码率自适应播放，编码格式需受浏览器支持。
- 视频下架会从站内列表与公开详情隐藏内容，但已知媒体直链仍可访问。
- 前端采用静态导出和查询参数路由，视频分享地址为 `/?view=watch&id=<视频 ID>`。

## 相关文档

- [管理后台使用说明](docs/admin.md)
- [前端设计](docs/frontend-redesign.md)
- [导航与加载性能](docs/navigation-performance.md)
- [后端源码来源说明](backend/UPSTREAM.md)
