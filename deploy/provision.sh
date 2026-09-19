#!/usr/bin/env bash
set -euo pipefail
if [[ $(id -u) -ne 0 ]]; then echo 'run as root' >&2; exit 1; fi
if [[ $# -ne 2 ]]; then echo 'usage: provision.sh REPO_DIR CI_PUBLIC_KEY_FILE' >&2; exit 1; fi
repo=$(realpath "$1")
pubkey=$(realpath "$2")
[[ -f "$repo/deploy/deploy.py" && -f "$pubkey" ]] || { echo 'missing repo or key' >&2; exit 1; }
# Swap is an emergency cushion. The acceptance test still requires stable RAM use.
if ! swapon --show=NAME | grep -qx /swapfile; then
  if [[ ! -f /swapfile ]]; then fallocate -l 2G /swapfile; chmod 600 /swapfile; mkswap /swapfile; fi
  swapon /swapfile
  grep -q '^/swapfile ' /etc/fstab || printf '/swapfile none swap sw 0 0\n' >> /etc/fstab
fi

export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get install -y mysql-server redis-server rabbitmq-server caddy rsync curl python3

getent group owlet-video >/dev/null || groupadd --system owlet-video
id owlet-video >/dev/null 2>&1 || useradd --system --gid owlet-video --home-dir /var/lib/owlet-video --shell /usr/sbin/nologin owlet-video
id owlet-video-ci >/dev/null 2>&1 || useradd --create-home --shell /bin/bash owlet-video-ci
install -d -o owlet-video -g owlet-video -m 0750 /var/lib/owlet-video /var/lib/owlet-video/media /var/lib/owlet-video/tmp
install -d -o owlet-video-ci -g owlet-video-ci -m 0755 /srv/owlet-video /srv/owlet-video/releases
install -d -o root -g owlet-video -m 0750 /etc/owlet-video
install -d -o root -g root -m 0700 /var/backups/owlet-video
usermod -aG owlet-video caddy

if [[ ! -f /etc/owlet-video/app.env ]]; then
  dbpass=$(openssl rand -hex 24)
  redispass=$(openssl rand -hex 24)
  rabbitpass=$(openssl rand -hex 24)
  jwtsecret=$(openssl rand -hex 32)
  cat > /etc/owlet-video/app.env <<EOF
ADDR=127.0.0.1:8080
MYSQL_DSN='owlet_video:${dbpass}@tcp(127.0.0.1:3306)/owlet_video?charset=utf8mb4&parseTime=True&loc=UTC'
REDIS_ADDR=127.0.0.1:6379
REDIS_PASSWORD=${redispass}
RABBITMQ_URL='amqp://owlet_video:${rabbitpass}@127.0.0.1:5672/owlet_video'
JWT_SECRET=${jwtsecret}
PUBLIC_URL=https://owl-et.me
DATA_DIR=/var/lib/owlet-video
SECURE_COOKIES=true
MAX_MEDIA_BYTES=8589934592
EOF
  chown root:owlet-video /etc/owlet-video/app.env
  chmod 0640 /etc/owlet-video/app.env
  cat > /etc/owlet-video/mysql-backup.cnf <<EOF
[client]
user=owlet_video
password=${dbpass}
host=127.0.0.1
EOF
  chmod 0600 /etc/owlet-video/mysql-backup.cnf
fi

set -a
# app.env is root-owned, generated here with restricted permissions.
source /etc/owlet-video/app.env
set +a
dbpass=${MYSQL_DSN#owlet_video:}; dbpass=${dbpass%%@tcp*}
rabbitpass=${RABBITMQ_URL#amqp://owlet_video:}; rabbitpass=${rabbitpass%%@*}

cat > /etc/mysql/mysql.conf.d/owlet-video.cnf <<'EOF'
[mysqld]
bind-address=127.0.0.1
innodb_buffer_pool_size=128M
max_connections=30
performance_schema=OFF
EOF
systemctl enable --now mysql
systemctl restart mysql
mysql <<SQL
CREATE DATABASE IF NOT EXISTS owlet_video CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
CREATE USER IF NOT EXISTS 'owlet_video'@'127.0.0.1' IDENTIFIED BY '${dbpass}';
ALTER USER 'owlet_video'@'127.0.0.1' IDENTIFIED BY '${dbpass}';
GRANT ALL PRIVILEGES ON owlet_video.* TO 'owlet_video'@'127.0.0.1';
SQL

if ! grep -q '^# owlet-video settings$' /etc/redis/redis.conf; then
  cat >> /etc/redis/redis.conf <<EOF
# owlet-video settings
bind 127.0.0.1 ::1
requirepass ${REDIS_PASSWORD}
maxmemory 64mb
maxmemory-policy allkeys-lru
EOF
fi
systemctl enable --now redis-server
systemctl restart redis-server

if ! grep -q '^# owlet-video settings$' /etc/rabbitmq/rabbitmq.conf 2>/dev/null; then
  cat >> /etc/rabbitmq/rabbitmq.conf <<'EOF'
# owlet-video settings
listeners.tcp.1 = 127.0.0.1:5672
vm_memory_high_watermark.absolute = 256MiB
disk_free_limit.absolute = 1GB
EOF
fi
systemctl enable --now rabbitmq-server
systemctl restart rabbitmq-server
rabbitmqctl add_vhost owlet_video 2>/dev/null || true
rabbitmqctl add_user owlet_video "$rabbitpass" 2>/dev/null || rabbitmqctl change_password owlet_video "$rabbitpass"
rabbitmqctl set_permissions -p owlet_video owlet_video '.*' '.*' '.*'

install -d -o root -g root -m 0755 /usr/local/libexec
install -o root -g root -m 0755 "$repo/deploy/deploy.py" /usr/local/libexec/owlet-video-deploy.py
install -o root -g root -m 0755 "$repo/deploy/backup.sh" /usr/local/sbin/owlet-video-backup
install -o root -g root -m 0644 "$repo/deploy/owlet-video-api.service" /etc/systemd/system/owlet-video-api.service
install -o root -g root -m 0644 "$repo/deploy/owlet-video-worker.service" /etc/systemd/system/owlet-video-worker.service
install -o root -g root -m 0644 "$repo/deploy/owlet-video-backup.service" /etc/systemd/system/owlet-video-backup.service
install -o root -g root -m 0644 "$repo/deploy/owlet-video-backup.timer" /etc/systemd/system/owlet-video-backup.timer
install -o root -g root -m 0644 "$repo/deploy/Caddyfile" /etc/caddy/Caddyfile
caddy validate --config /etc/caddy/Caddyfile

install -d -o owlet-video-ci -g owlet-video-ci -m 0700 /home/owlet-video-ci/.ssh
printf 'command="/usr/bin/python3 /usr/local/libexec/owlet-video-deploy.py",restrict %s\n' "$(cat "$pubkey")" > /home/owlet-video-ci/.ssh/authorized_keys
chown owlet-video-ci:owlet-video-ci /home/owlet-video-ci/.ssh/authorized_keys
chmod 0600 /home/owlet-video-ci/.ssh/authorized_keys
cat > /etc/sudoers.d/owlet-video-ci <<'EOF'
owlet-video-ci ALL=(root) NOPASSWD: /usr/bin/systemctl restart owlet-video-api.service, /usr/bin/systemctl restart owlet-video-worker.service
EOF
chmod 0440 /etc/sudoers.d/owlet-video-ci
visudo -cf /etc/sudoers.d/owlet-video-ci
systemctl daemon-reload
systemctl enable owlet-video-api.service owlet-video-worker.service owlet-video-backup.timer
systemctl start owlet-video-backup.timer
echo 'Provisioned. Deploy a release, then enable/start Caddy after health checks.'
