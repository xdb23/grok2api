# Grok2API-XDB Fork 改造计划

- 上游：`chenyme/grok2api` @ `v3.0.8-hotfix.1` (`dae50ce`)
- 本仓：`https://github.com/xdb23/grok2api`
- 本地：`/opt/g2a-xdb/grok2api`
- 交付：Docker 同构栈 + 私库分发（非单二进制焊死注册）

## 1. 目标

1. **统一产品面**：号池 / 出口 / 注册 / 转换 / 导出 / 多机状态，尽量一个管理端看完。
2. **保留成熟注册逻辑**：现有 Python 注册 worker（Docker）作为 sidecar，方法与协议不重写。
3. **角色**：
   - `standalone`：完整可用
   - `node`：完整可用 + 上报主控 + 接受指令
   - `controller`：完整可用 + 聚合多节点 + 控制/统计/权限
4. **增强 Web→Build**：对齐生产 mint（`referrer=grok-build`、浏览器头、代理策略）。
5. **导出**：CPA / sub2api 友好格式（在官方 export 之上适配）。

## 2. 官方源码结构（与扩展点）

```
backend/
  cmd/grok2api          # 入口
  internal/app          # 装配根（新模块在此接线）
  internal/application  # 用例：account / egress / settings / gateway ...
  internal/domain
  internal/repository   # 接口 + Redis 锁/总线
  internal/infra        # GORM / provider(web|cli|console) / egress
  internal/transport/http  # Gin：/api/admin/v1 + /v1
frontend/src/features/* # 管理端页面脚手架
docker-compose.yml      # profile 侧车（warp/flaresolverr）
```

**推荐扩展方式（侵入最小）**

| 优先级 | 方式 |
|--------|------|
| 1 | 新垂直切片：`domain/X` + `repository/X` + `application/X` + `transport/http/X`，仅 `app.New` + `server.go` 接线 |
| 2 | 复用 Redis 锁/总线、account 导入落库 |
| 3 | Compose `profiles: [register]` 挂 worker |
| 避免 | 改 gateway 热路径、滥用 Admin JWT 给 worker |

**建议新包**

```
domain/register, domain/cluster
application/register, application/cluster
transport/http/register, transport/http/cluster
infra/persistence/relational/register_*.go, cluster_*.go
```

**鉴权面**

- 人：现有 Admin JWT（`/api/admin/v1`）
- Worker / 节点：新建 `/api/worker/v1` 或 `/api/node/v1` + 独立 token（勿复用 client key）

## 3. Web→Build / Console（源码结论）

| 路径 | 行为 | 代理 |
|------|------|------|
| Web→Build | SSO 驱动 Device OAuth（device/code→verify→approve→token） | Web egress；**可直连**（不强制代理） |
| Web→Console | **同一 SSO 投影**，无 Device mint | 后续打 Console 上游才用 egress |

Convert 细节：

- Web scope 使用 **tls-client 浏览器 TLS 指纹**（优于「完全无指纹」的旧判断）
- 仍缺生产 mint 常见项：`referrer=grok-build`、Origin/Referer/Sec-Fetch/Client-Hints
- 限制：指定 ID 单次 ≤1000；`conversionConcurrency` 默认 25
- 导出：`GET /accounts/export?provider=grok_build|grok_web|grok_console`（≤10000）

**最小 convert 补丁文件**：`backend/internal/infra/provider/web/sso_build.go`

## 4. Docker 交付

- 官方：多阶段 → **单镜像**（Go + 前端 dist）
- XDB 栈建议：

```yaml
services:
  grok2api:          # fork 镜像
  register-worker:   # 现有成熟注册镜像/命令，profile: register
  # 可选 postgres/redis（多副本时）
```

- 私库：本机验通 → push → 他机 `pull` + 环境变量/角色配置
- 官方多副本（PG+Redis）= **网关对等副本**，≠ 注册控制面；控制面另做 `cluster` 域

## 5. 分期实施

### P0 — 仓与交付（已完成 / 进行中）

- [x] Fork 到 `xdb23/grok2api`
- [x] 本地克隆 + `upstream` remote
- [ ] 改 `docker-compose` 支持 build 本仓 + register profile 草图
- [ ] 私库命名约定（如 `registry.xdb23.../g2a-xdb:VERSION`）

### P1 — 薄增强（高价值、小 diff）

1. `sso_build.go`：device/code 加 `referrer=grok-build` + 浏览器头
2. 导出适配脚本或 API：`format=cpa|sub2api`（字段映射 + proxy_url 策略）
3. 管理端只读页：本机注册 worker 健康（HTTP 探测 / 日志摘要）

### P2 — 注册编排（产品耦合、进程旁路）

1. `application/register`：创建任务、回调落账号（走现有 account import）
2. Worker 回调鉴权
3. 前端 `features/register`：启停、水位、最近失败
4. Compose 挂现有 `grok-theyka-refill` / producer 类容器

### P3 — 多机控制面

1. `cluster_nodes` 表：心跳、版本、摘要（号量、注册速率、CPU）
2. `MODE=node` 定时上报 controller
3. Controller Admin：节点列表、只读统计
4. 轻控制：暂停注册、触发一批（鉴权+审计）
5. 权限：先「主控管理员 / 节点 token」两级，细 RBAC 后置

### P4 — 官方同步

- `git fetch upstream && git merge upstream/main` 按需
- 优先 cherry-pick 安全/协议修复；冲突集中在 `sso_build` / 新 package / compose

## 6. 明确不做（首期）

- 把 Playwright/邮箱/Turnstile 重写进 Go 主进程
- 跨机自动搬号（号池真相在各节点本地）
- 一次替换现网全部 CPA（长期可并存：导出 + 双写）

## 7. 与现网关系

| 现网组件 | Fork 中位置 |
|----------|-------------|
| `/opt/grok2api` 旁路 canary | 继续可跑；改造在 `/opt/g2a-xdb` 验完再替换镜像 |
| `/opt/grok-platform` 注册 | 作为 register-worker 镜像源/挂载 |
| CPA | 保留；导出 CPA 格式对接 |

## 8. 验收草图

1. `docker compose up` standalone：健康检查 + 管理端登录
2. Web 导入 1 SSO → convert：token 含合理 claims / 可 refresh
3. 导出 Build JSON 能被现有 `import_auth_dir.py` 或 CPA 接受（字段对齐后）
4. 两机：node 心跳出现在 controller 列表
5. 断网 controller：node 仍可本地注册与推理

---

文档版本：2026-07-24  
分析方式：本机源码 + 3 路 explore 子代理（架构 / convert / Docker+前端）
