# 本机热重载开发

改协议 / 管理端时**不要每次 `docker build`**。本机用 `air`（Go）热重载，稳定后再打镜像。

## 公网域名（推荐日常开发）

现网 OpenResty：

`https://grok2api.xdb23.com` → `http://127.0.0.1:18000`

**域名不用改。** 停掉 Docker 后，本机 `air` 占同一端口即可：

```bash
cd /opt/g2a-xdb/grok2api
make dev-api    # 停容器 + restart=no + air @18000
```

之后浏览器继续打开：https://grok2api.xdb23.com

| 项目 | 影响 |
|------|------|
| 公网域名 | 无影响（仍反代 18000） |
| 账号 / 密钥 | 无影响（共用 volume SQLite） |
| resin 出口 | hosts + socat 转 2260 |
| 容器 | `restart=no` 且已 stop，不会抢端口 |
| 恢复镜像 | `make docker-up`（会先停本机 dev） |

公网走 Go 托管的 `frontend/dist`。改前端 UI 时本机可另开 `make dev-web`（:5173 HMR），域名仍打 dist，前端要 `pnpm build` 一次才进域名。

## 地址

| 地址 | 用途 |
|------|------|
| https://grok2api.xdb23.com | 公网管理台 + API |
| http://127.0.0.1:18000 | 本机直连 |
| http://127.0.0.1:5173 | 可选 Vite HMR |

## 常用命令

```bash
make dev-api          # 公网开发（推荐）
make dev              # API + Vite 本机 HMR
make dev-web          # 仅 Vite
make dev-stop         # 停本机 air
make docker-up        # 恢复 Docker 现网
make docker-build     # 稳定后打包
```

## 工作流

1. `make dev-api` → 域名上点管理台 / 测 convert  
2. 改 `backend/**/*.go`，保存后 air 约 1–5s 重启  
3. 满意 → `make docker-build && make docker-up`

## 配置

- `config.dev.yaml`：本机密钥 + volume 数据路径（gitignore）
- 模板：`config.dev.example.yaml`
- 日志：`/tmp/g2a-dev-api.log`
