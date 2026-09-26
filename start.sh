#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"
command -v docker >/dev/null || { echo 'Docker is required' >&2; exit 1; }
services=(mysql redis rabbitmq api)
[[ "${START_WORKER:-1}" == 0 ]] || services+=(worker)
[[ "${START_FRONTEND:-1}" == 0 ]] || services+=(web)
stop() { if [[ "${STOP_DOCKER:-0}" == 1 ]]; then docker compose stop "${services[@]}"; fi; }
trap stop EXIT
trap 'exit 130' INT TERM
docker compose up -d --build "${services[@]}"
docker compose logs -f "${services[@]}"
