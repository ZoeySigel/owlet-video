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
