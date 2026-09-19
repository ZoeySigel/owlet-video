#!/usr/bin/env bash
set -euo pipefail
umask 077
exec 9>/run/lock/owlet-video-backup.lock
flock -n 9 || exit 0
if [[ -x /srv/owlet-video/current/bin/owlet-video ]]; then
  set -a
  source /etc/owlet-video/app.env
  set +a
  runuser -u owlet-video -- /srv/owlet-video/current/bin/owlet-video cleanup || true
fi
base=/var/backups/owlet-video
stamp=$(date -u +%Y%m%dT%H%M%SZ)
mkdir -p "$base"
tmp="$base/.${stamp}.tmp"
mkdir -p "$tmp/media"
trap 'rm -rf -- "$tmp"' EXIT
mysqldump --defaults-extra-file=/etc/owlet-video/mysql-backup.cnf --single-transaction --no-tablespaces --set-gtid-purged=OFF owlet_video | gzip -9 > "$tmp/mysql.sql.gz"
previous=$(find "$base" -mindepth 1 -maxdepth 1 -type d -name '20*' | sort | tail -n 1 || true)
if [[ -n "$previous" ]]; then
  rsync -a --link-dest="$previous/media" /var/lib/owlet-video/media/ "$tmp/media/"
else
  rsync -a /var/lib/owlet-video/media/ "$tmp/media/"
fi
mv -- "$tmp" "$base/$stamp"
trap - EXIT
find "$base" -mindepth 1 -maxdepth 1 -type d -name '20*' -mtime +6 -exec rm -rf -- {} +
