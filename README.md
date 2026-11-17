# Lumeo · 摄影商单全链路工作流平台

> 面向个人摄影师与工作室的商单系统：线索 → 报价 → 合同 → 预约 → 拍摄 → 采集 → 选片 → 修图 → 审片 → 交付 → 收款 → 归档，一个平台跑完整个拍摄生意。

[![CI](https://github.com/Troy-JB/Photography-Workflow/actions/workflows/ci.yml/badge.svg)](https://github.com/Troy-JB/Photography-Workflow/actions/workflows/ci.yml)
[![Agent Windows](https://github.com/Troy-JB/Photography-Workflow/actions/workflows/agent-windows.yml/badge.svg)](https://github.com/Troy-JB/Photography-Workflow/actions/workflows/agent-windows.yml)
![Go](https://img.shields.io/badge/backend-Go-00ADD8?logo=go&logoColor=white)
![Vue](https://img.shields.io/badge/frontend-Vue%203%20%2B%20TS-4FC08D?logo=vuedotjs&logoColor=white)

## 项目简介

Lumeo 是一套为摄影师打造的**商单全链路工作流平台**，从第一天起就按多租户 SaaS 架构设计。它把接单过程中的商务谈判、电子签约、档期管理、素材采集、客户选片、审片返修、成品交付与财务对账全部搬到线上，让摄影师不再需要在微信、Excel、网盘和记账工具之间来回切换。

## 核心特性

### 全链路业务闭环

- **商务与电子签**：线索跟进、版本化报价（服务端计价、快照冻结）、合同生成与电子签署回调，签署文件哈希落库可验
- **支付与账务**：定金 / 尾款 / 全款 / 变更补款订单，回调验签入账、退款、对账、关单恢复、支付安全告警与发票管理
- **排期与拍前准备**：资源周规则、可用性查询、TTL 锁位、定金到账自动确认、改期取消、shot list / 器材 / 授权与 call sheet 冻结
- **素材采集**：Windows 采集 Agent 直传对象存储（字节不经过 API），内容哈希去重，批量导入崩溃可恢复
- **图库与选片**：客户 H5 图库分享链接、水印派生图管线、选片截止提醒、不可变发布清单
- **审片与返修**：多轮审片状态机、逐图批注评论、修图版本管理、外部修图协作对接
- **交付与归档**：构建流水线生成下载包、尾款闸门控制下载授权、自动归档与保留期管理
- **客户自助**：微信小程序客户中心（登录绑定工作室、在线预约、我的拍摄），H5 打开选片 / 审片 / 交付链接

### 平台与工程能力

- **多租户安全**：所有业务表带 `tenant_id`，PostgreSQL 行级安全（RLS）双重防御，API / Worker / 迁移使用相互隔离的数据库角色与最小授权
- **可靠异步**：数据库任务队列（`FOR UPDATE SKIP LOCKED`、租约回收、指数退避）+ 事务性 Outbox 事件，业务事件不丢
- **写操作幂等**：全量创建类接口支持 `Idempotency-Key`，重放返回首次响应
- **适配器架构**：支付、电子签均为 Provider 接口 + 内置 HMAC sandbox 实现；生产环境启动守卫拒绝以 sandbox 适配器处理真实资金与合同
- **可观测性**：Prometheus 指标、W3C `traceparent` 透传、结构化日志、审计日志、统一 `problem+json` 错误
- **契约先行**：OpenAPI 契约（`shared/openapi.yaml`）驱动接口测试
- **交付质量**：单测 + 集成 + Playwright E2E + 压测（选片写 / 200 并发抢档 p95 阈值）+ 支付回调重放 + 迁移矩阵，全部纳入 GitHub Actions CI（含 Windows Agent 的 GUI 构建与 NSIS 打包）

## 系统架构

```
                    ┌─────────────────────────┐
   摄影师 ──────────▶  web-admin (管理端+官网)  │──┐
   客户（小程序）────▶  web-client-miniapp       │  │
   客户（分享链接）──▶  web-gallery (H5 图库)    │  │ Vue 3 + TS + Vite / uni-app
                    └─────────────────────────┘  │
                                 │               │
                    ┌────────────▼───────────────▼───┐
                    │        API (Go, :8080)          │
                    │  JWT 认证 · 幂等 · RLS · 审计    │
                    └───┬──────────┬──────────────┬──┘
                        │          │ 预签名 URL    │ Webhook（支付/电子签）
                        ▼          ▼              ▼
                 PostgreSQL 16   MinIO 对象存储   外部渠道
                   (RLS)
                        ▲
                        │ 任务队列 / Outbox
                    ┌───┴────────────┐
                    │  Worker (Go)   │  校验·投递·交付构建·归档·清扫
                    └────────────────┘

   拍摄现场 ──────────▶ Windows 采集 Agent（Wails GUI / 无头 CLI，直传对象存储）
```

**六个子系统**

| 组件 | 说明 |
|---|---|
| `server/` | Go 后端：HTTP API、异步 Worker、迁移、种子、E2E smoke、压测 CLI |
| `agent/` | Windows 采集 Agent：Wails 桌面端 + 无头 CLI，Ed25519 更新签名 + Authenticode 校验，NSIS 安装包 |
| `web-admin/` | 管理端与 Lumeo 官网：工作台、线索、报价合同、排期、图库、审片、交付、归档 |
| `web-gallery/` | 客户 H5：选片 / 审片 / 交付分享页 |
| `web-client-miniapp/` | 客户中心：微信小程序预约 + H5 |
| `shared/` | OpenAPI 契约 |

## 快速开始

### Docker 一键启动

```bash
docker compose -f deploy/docker-compose.yml up --build
```

首次启动会自动构建 API、Worker、Postgres、MinIO、三个前端并初始化迁移与演示账号：

- 管理端（含官网）：<http://localhost:5175> — `owner@example.com` / `photographer123`
- 客户中心：<http://localhost:5173>
- 客户图库分享页：<http://localhost:5174>
- API 健康检查：<http://localhost:8080/healthz>

### 本机开发

前置：Go 1.26+、Node.js 22+、Docker Desktop。

```bash
make up        # 起依赖（postgres / redis / minio / gotenberg）
make migrate   # 建表
make seed      # 演示租户与账号
make api       # 启动 API（:8080）
make worker    # 另开终端：异步 Worker
make smoke     # 端到端全流程验证

# 前端（各开一个终端）
cd web-admin && npm ci && npm run dev
cd web-gallery && npm ci && npm run dev
cd web-client-miniapp && npm ci && npm run dev:h5
```

更多细节见 [docs/dev-progress.md](docs/dev-progress.md)（模块设计与实现细节）、[docs/dr.md](docs/dr.md)（备份与灾备演练手册）、[docs/runbook-delivery-archive.md](docs/runbook-delivery-archive.md)（交付归档故障处置手册）。

## 技术栈

| 层 | 技术 |
|---|---|
| 后端 | Go、PostgreSQL 16（RLS + goose 迁移）、Redis、MinIO、Gotenberg |
| 前端 | Vue 3、TypeScript、Vite、uni-app（微信小程序） |
| 桌面端 | Wails、NSIS、Ed25519 签名更新、Authenticode |
| 测试 | Go 单测、Playwright E2E、压测 CLI、支付回调重放 |
| 交付 | Docker Compose、GitHub Actions（Linux CI + Windows Agent CI） |

## 目录结构

```
server/                  Go 后端（api / worker / migrate / seed / smoke / loadtest ...）
agent/                   Windows 采集 Agent（Wails GUI + 无头 CLI + NSIS 打包）
web-admin/               管理端 + Lumeo 官网（Vue 3）
web-gallery/             客户分享 H5（选片 / 审片 / 交付）
web-client-miniapp/      客户中心（uni-app 小程序）
shared/openapi.yaml      OpenAPI 契约
deploy/                  docker-compose 与数据库初始化
scripts/                 CI / E2E / 压测 / 重放 / 灾备演练脚本
docs/                    开发文档 · 运维手册 · 灾备手册
.github/workflows/       CI 流水线
```

## 相关链接

- 详细设计与实现进度：[docs/dev-progress.md](docs/dev-progress.md)
- CI（服务端与前端）：[actions/workflows/ci.yml](.github/workflows/ci.yml)
- CI（Windows Agent）：[actions/workflows/agent-windows.yml](.github/workflows/agent-windows.yml)
