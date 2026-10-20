# GCFeed 后端验证

当前只运行 GCFeed。旧后端的快照榜单、时间线、互动命令、写入预算与种子数据程序及其专属测试已删除。旧容量结果在 capacity 目录归档，不能作为当前容量结论。

## 覆盖范围

- backend/test：GCFeed Feed、推荐、曝光、播放 QoS、账号、互动、关系、消息、上传、视频与向量生成。
- backend/core_integration_test.go：隔离旧库与新库迁移、旧模式较早的表结构、媒体与上传路径、保留会话及运行时兼容。验证真实发布事件到达向量 Worker。
- 根目录保留会话缓存/撤销/改名/改密、数据库故障超时、详情缓存请求合并/锁/旧值兜底、点赞游标、SSE 故障兜底及 Outbox 发布确认重试测试。集成夹具初始化 GCFeed 表与兼容视图，写入核心表。
- init-core：只接受空库或 verified GCFeed 库，旧库及部分迁移库被拒绝。

## 本地复现

在项目根目录启动独立依赖：

```powershell
docker compose -f compose.test.yml up -d --wait
$env:TEST_MYSQL_DSN = 'owlet_test:test-password@tcp(127.0.0.1:23306)/owlet_video_test?parseTime=True&charset=utf8mb4&loc=UTC'
$env:TEST_REDIS_ADDR = '127.0.0.1:26379'
$env:TEST_REDIS_PASSWORD = ''
$env:TEST_RABBITMQ_URL = 'amqp://owlet_test:test-password@127.0.0.1:25672/'
$env:CORE_TEST_MYSQL_DSN = 'root:test-root-password@tcp(127.0.0.1:23306)/mysql?parseTime=True&charset=utf8mb4&loc=UTC'
$env:CORE_TEST_REDIS_ADDR = $env:TEST_REDIS_ADDR
$env:CORE_TEST_RABBITMQ_URL = $env:TEST_RABBITMQ_URL
Set-Location backend
go vet ./...
go test -race -count=1 ./...
Set-Location ..
python -m unittest discover -s deploy -p 'test_*.py'
docker compose config --quiet
```

Linux 使用相同变量配合 export。竞争检测需要平台支持的 C 工具链。未提供对应 DSN 时集成测试会跳过；完整验证必须设置上述变量。迁移测试会创建并清理两个随机名称数据库，需要测试服务器管理员权限；不得连接生产数据库。Compose 测试库使用临时存储。

本轮改动在真实 MySQL、Redis、RabbitMQ 下通过完整 Go 测试。没有部署生产环境，也不据此声称生产吞吐、延迟或多节点容灾能力。
