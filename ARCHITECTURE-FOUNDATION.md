# G2A-XDB 基座架构（Foundation）

> 状态：设计稿（先打基座，再分阶段验证）  
> 日期：2026-07-24  
> 仓库：https://github.com/xdb23/grok2api  
> 本地：`/opt/g2a-xdb/grok2api`  
> 原则：**产品一体、进程旁路；权威状态进库；节点独立可活、主控可选。**

---

## 0. 先回答你的核心问题

| 问题 | 结论 |
|------|------|
| 注册怎么合并？ | **编排进 G2A 架构（DB/API/面板/策略）**；浏览器注册 **不重写进 Go**，保留成熟 Docker worker，由编排器调度。 |
| 能否像他一样用 PG/Redis？ | **能。** 单机默认 SQLite；多机/主控用 **Postgres + Redis**（对齐官方多副本契约）。 |
| 一个镜像搞定全部？ | **网关+控制面 = 一个镜像**；**注册浏览器栈 = 另一镜像（可 profile 同栈）**。不要把 Chrome/Xvfb 塞进网关镜像。 |
| CPU 高暂停注册？ | **必须保留**。现网 `RESERVE_CPU_GATE_*` 滞回逻辑迁到 **编排策略**，高负载不发新任务。 |
| 新注册产什么？ | **只产 Web（SSO）→ 直接写入 G2A `grok_web` 账号表**。不再在注册末强制 mint Build。 |
| Web→Build？ | **可配：手动 / 自动 / 半自动（入库后异步队列）**。官方默认是管理端手动；我们加策略开关。 |
| 主控/被控？ | 节点生成 **node token**；主控保存节点 URL+token 即可拉取/下发。节点离线仍全功能。 |
| 是否「确定没问题」？ | **设计上闭环，不等于未验证就没问题。** 基座打好后按 P0→P3 冒烟，每步可回滚。 |

**不能承诺的：** 一次上线就替代全部 CPA、零故障。  
**能承诺的设计：** 边界清楚、与官方扩展点一致、注册成熟逻辑可复用、主从可独立。

---

## 1. 目标行为（你要的「新注册」）

```
[Worker] 浏览器注册成功
    → 得到 SSO (+ email 等元数据)
    → POST 本机 G2A Worker API：入库 grok_web（直接 active）
    → （可选策略）入队 auto-convert → Build
    → 不再默认写备用库 JSON / 不再默认 mint Build 给 CPA

[管理端]
    → 看 Web 号池、注册任务、CPU 门控状态
    → 手动「转 Build」或依赖自动转换
    → 导出 CPA/sub2api（从 Build 导出，需要时再转）
```

与旧链路对比：

| | 旧（现网） | 新（XDB 目标） |
|--|-----------|----------------|
| 注册终点 | SSO + **立刻 Device mint Build** → 文件备用库/CPA | **仅 Web SSO 入库** |
| 库存形态 | 磁盘 JSON + CPA auth 文件 | G2A DB（web/build 表） |
| Build | 注册时已有（会过期躺库） | 用时/策略转换，token 更新鲜 |
| CPU 门控 | producer 脚本 env | 编排器策略 + 面板可配 |
| 多机 | 五机各自脚本 | 同构栈 + 主控可选 |

---

## 2. 镜像与进程模型（关键：不是「一个镜像塞全世界」）

### 2.1 推荐：2 个业务镜像 + 可选基础设施

```
┌─────────────────────────────────────────────────────────┐
│  docker compose stack（每台机器同构）                      │
│                                                         │
│  [1] g2a-xdb          官方同构单镜像（Go API + 管理端）     │
│       · 推理网关 / 号池 / 转换 / 导出                      │
│       · 注册编排 API + 策略（CPU 门控）                    │
│       · 节点心跳 / 主控聚合（同一二进制，角色 env 区分）     │
│                                                         │
│  [2] g2a-register     现有注册逻辑镜像（Python+浏览器依赖） │
│       · 保留 mint/SSO 获取方法，去掉「强制 Build mint」     │
│       · 成功只回调 G2A：写入 Web                           │
│       · 可 0~N 副本；CPU 高时由编排器停发任务               │
│                                                         │
│  [3] postgres         可选（standalone 用 SQLite 可不上）  │
│  [4] redis            可选（多副本/主控队列建议上）         │
│  [5] resin 等         代理，可 external 网络接入            │
└─────────────────────────────────────────────────────────┘
```

| 镜像 | 内容 | 是否一个搞定「全部功能」 |
|------|------|------------------------|
| `g2a-xdb` | 网关+管理+编排+主从控制面代码 | **产品功能面：是**（注册执行除外） |
| `g2a-register` | 浏览器注册执行器 | **执行面：是注册专用** |
| 基础设施 | PG/Redis | 按角色启用 |

**为什么注册不并进 g2a-xdb 单镜像？**

1. 浏览器/Chrome 依赖巨大，拖垮网关镜像体积与安全面。  
2. 请求 CPU 高时，需要 **停的是注册容器**，不是把网关一起打死。  
3. 官方升级 merge 时，注册与网关冲突面分离。  
4. 现网已经是 Docker 注册，**保留逻辑 = 改回调与配置，不是重写。**

Compose 示例语义：

```yaml
# 伪代码
services:
  g2a:
    image: registry.xxx/g2a-xdb:${TAG}
    environment:
      G2A_ROLE: standalone|node|controller   # 控制面角色
      G2A_NODE_TOKEN: "..."                  # node 被控令牌（或启动生成）
      G2A_CONTROLLER_URL: "https://..."      # node 可选上报
    volumes: [ config, data ]
  register:
    image: registry.xxx/g2a-register:${TAG}
    profiles: ["register"]
    environment:
      G2A_WORKER_URL: http://g2a:8000
      G2A_WORKER_TOKEN: "..."
      # 原 RESERVE_* / 邮件 / resin 等迁移为统一配置
    # cpus / mem 限制可选，与门控互补
  postgres: ...   # ROLE!=standalone 或显式启用
  redis: ...
```

**「一个 compose 栈 pull 起来 = 全部功能」可以；「一个 OCI 镜像层包含 Chrome+网关」不建议。**

---

## 3. 数据层（对齐 G2A 架构，可 SQLite / PG + Redis）

### 3.1 模式

| 部署 | DB | Redis |
|------|-----|-------|
| 单机验证 | SQLite（与官方默认一致） | memory（可不上 Redis） |
| 生产单机加强 | Postgres | 可选 Redis |
| 多副本网关 / 主控队列 | **Postgres 必须** | **Redis 必须**（锁/总线/任务认领） |

权威状态：**永远在 SQL**；Redis 只做协调（与官方一致）。

### 3.2 新增表（基座，挂在 G2A 同一 DB）

```
register_policies          # CPU 门控阈值、是否自动转 Build、目标 ready 水位
register_jobs              # 一次注册任务（queued/running/success/failed/paused）
register_job_events        # 可选：阶段日志（sso_ok, import_ok, convert_ok）
register_workers           # 本机 worker 心跳（可选，单机可省略）
cluster_nodes              # 节点注册表（主控视角）
cluster_node_tokens        # 节点接入令牌哈希（主控侧）
cluster_commands           # 主控下发指令（pause_register / resume / convert_batch）
cluster_command_results    # 回执
```

**不新建「第二套账号表」。** Web/Build 继续走官方：

- `provider_accounts` + credentials（`grok_web` / `grok_build`）
- 关联表官方已有 `account_provider_links`

注册成功路径：

```
Worker → POST /api/worker/v1/register/complete
  body: { email, sso_token, meta... }
  → application/register
  → 复用 account.ImportWebCredentials / persist seed
  → job = success
  → 若 policy.auto_convert_build：投递 convert 任务（异步）
```

### 3.3 Redis 用途（有则用）

- 分布式锁：转换、任务 claim  
- 主控→多节点指令总线（可选）  
- 官方已有 invalidation / settings bus  

---

## 4. 注册合并方式（「重写」边界）

### 4.1 重写什么 / 不重写什么

| 层 | 策略 |
|----|------|
| 浏览器、Turnstile、邮箱、Resin sticky、SSO 获取 | **不重写**，保留现有 Python 实现 |
| 注册末 `mint_with_sso_protocol`（Device Build） | **默认关闭**；新链路只要 SSO |
| 写备用库 JSON / CPA import | **默认关闭**；改为 G2A Web 入库 |
| 任务状态、水位、CPU 门控、面板、主从 | **新建于 G2A（Go）**，架构对齐官方模块切法 |
| Web→Build | 优先调用官方 `ConvertToBuild`；可打补丁（referrer/头） |

这叫 **「编排重写 + 执行器保留」**，不是「整条注册用 Go 重写一遍」。

### 4.2 Worker 契约（基座 API）

**Worker 鉴权：** `Authorization: Bearer <worker_token>`  
（安装时生成，存 `register_workers` 或配置；**不是** Admin JWT，**不是** 客户端 API Key）

| API | 方向 | 作用 |
|-----|------|------|
| `POST /api/worker/v1/jobs/lease` | Worker←G2A | 领取 N 个注册任务（CPU 门控不通过则 lease 空） |
| `POST /api/worker/v1/jobs/{id}/heartbeat` | Worker→G2A | 运行中心跳 |
| `POST /api/worker/v1/jobs/{id}/complete` | Worker→G2A | SSO 成功 → **Web 入库** |
| `POST /api/worker/v1/jobs/{id}/fail` | Worker→G2A | 失败分类（turnstile/oauth/email…） |
| `GET  /api/worker/v1/policy` | Worker←G2A | 拉阈值/是否允许跑 |

管理端（人）：

| API | 作用 |
|-----|------|
| `GET/PUT /api/admin/v1/register/policy` | CPU 门控、自动转 Build、目标速率 |
| `POST /api/admin/v1/register/jobs` | 批量入队「注册 K 个」 |
| `POST /api/admin/v1/register/pause\|resume` | 全局暂停（与 CPU 自动门控叠加） |
| `GET /api/admin/v1/register/stats` | 成功率、暂停原因、队列深度 |

### 4.3 CPU 门控（迁移现网语义）

现网（`worker_reserve_producer.sh`）要点：

- `RESERVE_CPU_GATE_ENABLE`
- busy：`CPA_CPU>=150%` 或 `load1>=6` → pause  
- idle：`CPA_CPU<=80%` 且 `load1<=3` → resume  
- 滞回 + streak，避免抖  
- **只停新注册，不动推理进程**

XDB 基座：

```
policy:
  cpu_gate_enabled: true
  busy_load1: 6.0
  idle_load1: 3.0
  busy_cpa_cpu_pct: 150    # 可选：探测本机 cpa 容器；无 CPA 则只用 load/网关自测
  idle_cpa_cpu_pct: 80
  busy_streak: 2
  sample_seconds: 20
  pause_sleep_hint: 45
  # 互补：网关自身 CPU / 1m load
  busy_g2a_cpu_pct: 120
  max_inflight_register_jobs: 1..N
```

实现位置：`application/register` 在 **lease 任务前** 采样；busy → 不发 job，stats 显示 `paused_cpu`。  
Worker 侧可保留本地二次检查（双保险），但以编排器为准。

**与「请求 CPU 高」互补：**  
- 推理打满 → 停注册（保护 API）  
- 注册打满 → 可降 `max_inflight`（保护主机）  
- **绝不**在门控里重启网关或杀请求。

---

## 5. Web 入库与 Build 转换策略

### 5.1 入库（新默认）

成功注册 = 写入 **Provider=grok_web, AuthType=sso**。

字段最小集：`sso_token`, `email`, `name`；可选 CF cookie / tier。  
走官方 import 管线加密落库，避免自建账号存储。

### 5.2 转换模式（配置项 `convert_mode`）

| 模式 | 行为 | 适用 |
|------|------|------|
| `manual` | 仅管理端点「转 Build」 | 先观察 SSO 库存 |
| `auto` | complete 后异步 convert | 要立刻有 Build 可推理 |
| `on_demand` | 首次需要 Build 路由时再转 | 高级，后期 |

**官方本身是手动（管理端按钮 / API）**；`auto` 是我们编排层加的队列，底层仍调官方 Convert。

### 5.3 Convert 增强（P1 补丁，非基座阻塞）

- `referrer=grok-build`  
- 浏览器头补齐  
- 强制 Web egress 有健康节点才 convert（可选严格模式）

### 5.4 导出 CPA

- 仅 **Build** 导出可对 CPA  
- Web-only 号：先 convert 再导出  
- 适配层：`format=cpa` 补 `proxy_url` / 文件名规则（后期）

---

## 6. 主控 / 被控（必须写进基座）

### 6.1 角色

| G2A_ROLE | 本机能力 | 额外 |
|----------|----------|------|
| `standalone` | 完整网关+编排+注册 | 无集群 |
| `node` | 同上 | 向 controller 心跳；收命令 |
| `controller` | 同上 | 管理 `cluster_nodes`；聚合；下发 |

**每个角色本地都是完整 G2A**（可独立注册、独立推理）。  
主控挂了，node **继续本地跑**。

### 6.2 令牌模型（你说的「被控生成 token，主控填入」）

```
[在 Node 管理端]
  「接入主控」→ 生成 node_token（只显示一次）
  或安装脚本输出：
    NODE_ID=...
    NODE_URL=https://node.example:8443
    NODE_TOKEN=nopt_xxx

[在 Controller 管理端]
  「添加节点」表单：
    name, base_url, node_token
  → 存储 token 哈希；之后请求 Node 的 /api/node/v1/* 带 Bearer
```

双向可选：

- **主控拉（推荐先做）**：controller 定时 `GET {node}/api/node/v1/summary`  
- **节点推**：node 定时 `POST {controller}/api/admin/v1/cluster/heartbeat`（需 controller 发给 node 的 `controller_ingress_token`）

第一期只做 **主控拉 + 节点暴露只读 summary + 少量命令**。

### 6.3 节点 API（被控暴露）

鉴权：`NODE_TOKEN`

| API | 说明 |
|-----|------|
| `GET /api/node/v1/summary` | 版本、角色、号量 web/build、注册队列、CPU 门控状态、load |
| `GET /api/node/v1/register/stats` | 注册统计 |
| `POST /api/node/v1/register/pause\|resume` | 远程门控 |
| `POST /api/node/v1/register/jobs` | 远程入队 N 个（权限可关） |
| `POST /api/node/v1/convert/batch` | 远程触发 Web→Build（可选） |

主控不存对方账号 token 明文；**号池真相在各节点 DB**。  
主控只做统计与控制，不做「全局统一号库」（避免独立能力假象）。

### 6.4 权限（基座简化）

| 身份 | 权限 |
|------|------|
| Admin JWT | 本机全部 |
| Worker token | 仅 job lease/complete |
| Node token（主控持有） | summary + 策略允许的控制命令 |
| 客户端 API Key | 仅 `/v1` 推理（官方） |

细粒度 RBAC 后置。

---

## 7. 目录与代码落点（基座骨架）

```
backend/internal/
  domain/register/
  domain/cluster/
  application/register/     # policy, job FSM, cpu gate, complete→web import
  application/cluster/      # nodes, heartbeat aggregate, commands
  transport/http/register/
  transport/http/cluster/
  transport/http/worker/    # worker auth middleware
  transport/http/node/      # node token middleware
  infra/persistence/relational/register_*.go
  infra/persistence/relational/cluster_*.go

frontend/src/features/
  register/                 # 策略、队列、暂停原因
  cluster/                  # 节点列表、添加 token、汇总

deploy/
  docker-compose.xdb.yml
  config.standalone.example.yaml
  config.node.example.yaml
  config.controller.example.yaml
```

装配：仅 `app.New` + `server.go` 接线（官方扩展方式）。

---

## 8. 分阶段验证（基座打好后「一步一步」）

### Phase 0 — 仓与运行时（不接注册）

1. fork 可 build 镜像 `g2a-xdb`  
2. compose standalone 起得来，healthz/登录/原有账号功能正常  
3. **验收：** 与官方行为无回归  

### Phase 1 — 表 + Worker API + Web 入库（假 worker）

1. schema 新表 + migrate  
2. 用 curl 模拟 `complete`：写入 1 个 grok_web  
3. **验收：** 管理端能看到 Web 账号；无 Build  

### Phase 2 — 真注册 worker 打通（本机）

1. 改现有注册镜像：成功路径只回调 SSO，不 mint Build  
2. lease/complete 跑通 1 个真号  
3. **验收：** DB 有 web；日志无强制 Build mint  

### Phase 3 — CPU 门控

1. 人为抬高 load 或调低阈值 → lease 为空 / 状态 `paused_cpu`  
2. 降负载 → 自动 resume  
3. **验收：** 高负载不新开浏览器容器  

### Phase 4 — 转换策略

1. manual：按钮转 1 个 Build 成功  
2. auto：complete 后自动出现 Build  
3. **验收：** linked web↔build；可 export  

### Phase 5 — 主从

1. 两机 compose；node 生成 token；controller 添加节点  
2. summary 可见；远程 pause 生效  
3. **验收：** 断 controller，node 仍可注册  

### Phase 6 — 私库与他机

1. 推私库；第三台只 pull 部署  
2. **验收：** 同构可独立运行  

每阶段失败可停，不进入下一阶段。

---

## 9. 风险与「确定没问题吗」

| 风险 | 缓解 |
|------|------|
| 注册改回调引入回归 | 假 worker → 真 worker 灰度；保留旧 producer 可回切 |
| Web 入库后 convert 失败 | SSO 仍在，可重试 convert；不丢号 |
| 单镜像幻想导致网关不稳 | 强制双镜像边界 |
| 主控变单点 | 节点本地权威；主控仅增强 |
| 官方升级冲突 | 新包隔离；convert 补丁尽量小 |
| CPU 指标在容器内不准 | 优先主机 load 路径 / cgroup；与现网一致可挂 host proc |

**基座设计可以认为「结构没问题」；流程「没问题」只能由 Phase 0–5 证明。**

---

## 10. 与你原话的最终对齐

1. **Docker 交付、私库分发** → 是，compose 同构栈。  
2. **不是单二进制焊注册** → 是，双业务镜像。  
3. **PG/Redis 可按架构用** → 是，对齐官方多副本。  
4. **CPU 高暂停注册、互补不抢** → 是，策略进编排器。  
5. **新注册只产 Web 入库** → 是，默认路径。  
6. **Build 手动/自动可配** → 是（官方手动 + 我们 auto）。  
7. **主控填入被控 token** → 是，基座令牌模型。  
8. **先基座再逐步验证** → 见 §8，不一次焊死生产。

---

## 11. 建议的「下一步动作」（仍先设计落地顺序）

在你确认本基座后，实现顺序：

1. 在 fork 建空包 + schema + worker/node 鉴权中间件（无真注册）  
2. Phase 1 假 worker 打通 Web 入库  
3. 改 register 镜像回调  
4. CPU 门控  
5. convert 策略  
6. cluster token  

**在你明确说「按此基座开始写代码」之前，不改现网 `/opt/grok2api` 与生产 CPA。**
