#!/usr/bin/env bash
# 本机热重载开发入口：停 Docker G2A → 修好 resin 可达 → 起 API(air) + 前端(Vite HMR)
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

export PATH="${PATH}:/usr/local/go/bin:/root/go/bin:${HOME}/go/bin"
# nvm node if present
if [[ -d "${HOME}/.nvm/versions/node" ]]; then
  # shellcheck disable=SC2012
  LATEST_NODE="$(ls -1 "${HOME}/.nvm/versions/node" | sort -V | tail -1)"
  export PATH="${HOME}/.nvm/versions/node/${LATEST_NODE}/bin:${PATH}"
fi

MODE="${1:-all}" # all | api | web
API_PORT="${G2A_DEV_API_PORT:-18000}"
WEB_PORT="${G2A_DEV_WEB_PORT:-5173}"
CONFIG="${G2A_DEV_CONFIG:-${ROOT}/config.dev.yaml}"

log() { printf '\033[1;36m[dev]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[dev]\033[0m %s\n' "$*"; }
die() { printf '\033[1;31m[dev]\033[0m %s\n' "$*" >&2; exit 1; }

need() { command -v "$1" >/dev/null 2>&1 || die "缺少命令: $1"; }

ensure_tools() {
  need go
  if ! command -v air >/dev/null 2>&1; then
    log "安装 air（Go 热重载）…"
    go install github.com/air-verse/air@latest
  fi
  if [[ "$MODE" == "all" || "$MODE" == "web" ]]; then
    need node
    if ! command -v pnpm >/dev/null 2>&1; then
      log "安装 pnpm…"
      npm install -g pnpm@11.5.2
    fi
  fi
}

ensure_config() {
  if [[ ! -f "$CONFIG" ]]; then
    if [[ -f "${ROOT}/config.dev.example.yaml" ]]; then
      cp "${ROOT}/config.dev.example.yaml" "$CONFIG"
      warn "已从 config.dev.example.yaml 生成 $CONFIG，请核对密钥与数据路径后再用。"
    else
      die "缺少 $CONFIG。请复制 config.dev.example.yaml 或从 /opt/grok2api/config.yaml 生成。"
    fi
  fi
}

stop_docker_g2a() {
  if docker ps -a --format '{{.Names}}' 2>/dev/null | grep -qx grok2api; then
    # 关掉自动重启，避免本机开发时容器被拉起抢 18000 / 写库
    docker update --restart=no grok2api >/dev/null 2>&1 || true
    if docker ps --format '{{.Names}}' 2>/dev/null | grep -qx grok2api; then
      log "停止 Docker 容器 grok2api（释放 18000，避免双写 SQLite）…"
      docker stop grok2api >/dev/null
    else
      log "Docker grok2api 已停止（restart=no）"
    fi
  else
    log "未找到 Docker 容器 grok2api"
  fi
}

# 出口代理库里写的是 http://...@resin:2260；容器内靠 resin_default DNS。
# 本机把 resin → 127.0.0.1，并把 2260 转到 resin 映射端口 3030。
ensure_resin_host() {
  if ! grep -qE '[[:space:]]resin([[:space:]]|$)' /etc/hosts 2>/dev/null; then
    log "写入 /etc/hosts: 127.0.0.1 resin"
    echo '127.0.0.1 resin' >> /etc/hosts
  fi
  local resin_pub
  resin_pub="$(docker port resin 2260 2>/dev/null | head -1 | awk -F: '{print $NF}')"
  resin_pub="${resin_pub:-3030}"
  if ss -lntp 2>/dev/null | grep -qE ":2260\\b"; then
    log "本机 :2260 已在监听（resin 代理口）"
    return 0
  fi
  if command -v socat >/dev/null 2>&1; then
    log "转发 127.0.0.1:2260 → 127.0.0.1:${resin_pub}（resin）"
    nohup socat TCP-LISTEN:2260,bind=127.0.0.1,fork,reuseaddr "TCP:127.0.0.1:${resin_pub}" \
      >/tmp/g2a-resin-socat.log 2>&1 &
    echo $! > /tmp/g2a-resin-socat.pid
    sleep 0.3
  else
    warn "未安装 socat，无法把 :2260 转到 resin 映射口 ${resin_pub}。"
    warn "可: apt-get install -y socat   或把 resin 发布为 2260:2260"
  fi
}

start_api() {
  log "启动后端热重载 air → http://127.0.0.1:${API_PORT}"
  cd "$ROOT"
  # air args 里已写 config；用环境覆盖端口时改 .air 或下面直接 run
  if [[ "$API_PORT" != "18000" ]]; then
    exec air -c .air.toml -- --config "$CONFIG" --listen "127.0.0.1:${API_PORT}"
  fi
  exec air -c .air.toml
}

start_web() {
  log "启动前端 Vite HMR → http://127.0.0.1:${WEB_PORT}  (API → :${API_PORT})"
  cd "${ROOT}/frontend"
  if [[ ! -d node_modules ]]; then
    log "pnpm install…"
    pnpm install
  fi
  export VITE_DEV_API_TARGET="http://127.0.0.1:${API_PORT}"
  exec pnpm dev --host 127.0.0.1 --port "${WEB_PORT}"
}

start_all() {
  stop_docker_g2a
  ensure_resin_host
  log "后台启动 API…"
  (
    cd "$ROOT"
    air -c .air.toml
  ) > /tmp/g2a-dev-api.log 2>&1 &
  echo $! > /tmp/g2a-dev-api.pid
  # 等健康
  for i in $(seq 1 60); do
    if curl -sf "http://127.0.0.1:${API_PORT}/healthz" >/dev/null 2>&1; then
      log "API 已就绪 (healthz ok)"
      break
    fi
    if [[ "$i" -eq 60 ]]; then
      warn "API 尚未就绪，请看 /tmp/g2a-dev-api.log"
      tail -30 /tmp/g2a-dev-api.log || true
    fi
    sleep 0.5
  done
  log "前端日志 /tmp/g2a-dev-web.log ；API 日志 /tmp/g2a-dev-api.log"
  log "管理台: http://127.0.0.1:${WEB_PORT}  |  纯 API: http://127.0.0.1:${API_PORT}"
  cd "${ROOT}/frontend"
  if [[ ! -d node_modules ]]; then
    pnpm install
  fi
  export VITE_DEV_API_TARGET="http://127.0.0.1:${API_PORT}"
  pnpm dev --host 127.0.0.1 --port "${WEB_PORT}" 2>&1 | tee /tmp/g2a-dev-web.log
}

cleanup_all() {
  if [[ -f /tmp/g2a-dev-api.pid ]]; then
    kill "$(cat /tmp/g2a-dev-api.pid)" 2>/dev/null || true
    rm -f /tmp/g2a-dev-api.pid
  fi
  pkill -f 'tmp/grok2api-dev' 2>/dev/null || true
  pkill -f 'air -c .air.toml' 2>/dev/null || true
}

case "$MODE" in
  all)
    ensure_tools
    ensure_config
    trap cleanup_all EXIT INT TERM
    start_all
    ;;
  api)
    ensure_tools
    ensure_config
    stop_docker_g2a
    ensure_resin_host
    start_api
    ;;
  web)
    ensure_tools
    start_web
    ;;
  stop)
    cleanup_all
    log "本机 dev 进程已停。恢复 Docker: cd /opt/grok2api && docker compose up -d grok2api"
    ;;
  *)
    die "用法: $0 {all|api|web|stop}"
    ;;
esac
