# Owlet Video

一处收藏流动影像与微小冒险的地方。项目参考 [feedsystem_video_go](https://github.com/LeoninCS/feedsystem_video_go) 的功能范围，Go 后端与 Next.js 前端独立实现。

## 技术栈

- Go、Gin、GORM、MySQL 8.4：REST API、事务、索引、持久化。
- Redis 7：Lua 限流、视频缓存、Feed ZSET；MySQL 为业务数据来源。
- RabbitMQ 4：持久队列、发布确认、手动 Ack、重试、死信队列；事务性 Outbox + 幂等消费。
- Next.js 16、React 19、TypeScript：静态导出，生产环境由 Caddy 托管。

## 本地运行

需要 Docker Desktop / Docker Compose。复制 `.env.example` 为 `.env`，替换各个本地密码，然后执行：

```bash
docker compose up -d --build
docker compose exec api /usr/local/bin/owlet-video invite
```

一次性邀请码只在上述命令的终端显示。打开 <http://localhost:3000>，使用邀请码注册。RabbitMQ 管理界面位于本机 `127.0.0.1:15672`。不要把 `.env`、邀请码或上传内容提交到 Git。

## 功能与接口

访客可浏览最新、热度、点赞和话题 Feed、视频、用户主页及评论。受邀注册用户可分片上传和发布 MP4（每片 5 MiB、单文件最高 200 MiB）、点赞、评论、关注、私信、查看通知及 SSE 实时更新。前端的分享链接使用 `/?view=watch&id=<视频ID>`，静态文件可直接响应刷新。

API 前缀为 `/api/v1`：

| 范围 | 路径 |
| --- | --- |
| 账号 | `POST /auth/register`、`/auth/login`、`/auth/refresh`、`/auth/logout`；`GET /auth/me`；`PATCH /auth/password` |
| 用户 | `GET /users/:id`、`/users/:id/videos`、粉丝与关注；`PATCH /me`、`POST /me/avatar` |
| 视频 | `POST /uploads`、`PUT /uploads/:id/chunks/:index`、`POST /uploads/:id/complete`、`POST /videos`；`GET /videos`、`/videos/:id`、`/tags/:tag/videos` |
| 互动 | `PUT/DELETE /videos/:id/like`、评论、关注、`GET /me/likes` |
| 沟通 | `GET/POST /messages`、`GET/PATCH /notifications`、`GET /notifications/stream` |

部署和恢复步骤见 [运维文档](docs/operations.md)，数据流与一致性见 [架构文档](docs/architecture.md)。

## 验证

```bash
cd backend
go vet ./...
go test -race -count=1 ./...

cd ../frontend
npm ci
npm run typecheck
npm run build
```

Go 集成测试在配置 `TEST_MYSQL_DSN`、`TEST_REDIS_ADDR` 与 `TEST_RABBITMQ_URL` 时运行；GitHub Actions 会启动三个依赖并执行完整测试。
