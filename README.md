# 摄影商单全链路工作流平台

面向个人摄影师、从第一天起支持多租户 SaaS 的商单系统：线索 → 报价 → 合同 → 预约 → 拍摄 → 采集 → 选片 → 修图 → 审片 → 交付 → 收款 → 归档。

设计文档见 `C:\Users\Administrator\.qoder\plans\crisp-tor-chub.md`（二十二节，含采集 Agent、图库 H5、支付闭环、审片返修、自助预约）。

## 当前进度：M0 平台骨架 + M1 商务与电子签 + M2 支付账务（含发票、变更单、安全告警）+ M3 排期核心与拍前准备 + M4 Windows Agent + M5 图库与选片 + M6 后期集成与审片 + M7 交付下载与归档（全部已交付）

M0 已实现：

- 多租户数据模型 + Postgres RLS（API 以非超级用户 `pwf_app` 连接，`FORCE ROW LEVEL SECURITY` 真正生效）
- 账号体系：argon2id 密码、JWT access（15m）+ 一次性 refresh（落库可吊销）
- 项目生命周期状态机：11 个状态（`draft → quoted → contract_pending → contracted → booked → shooting → post_production → completed → archived`，另有 `on_hold`/`cancelled`），强制 `expected_version` 乐观锁，非法迁移/过期版本/闸门未满足一律 409
- 合同与收款以独立聚合演进，项目上只保留 `contract_status`/`payment_status` 投影；`contracted`/`booked`/`completed` 由投影值做闸门校验
- 存储抽象（`Storage` 接口）+ MinIO 预签名（单次 PUT / 分片 multipart）
- Agent 采集接口：开批次 → 500 条批量 upsert（内容哈希幂等）→ 预签名 → 完成 → finalize 封批并投递异步校验 job → 资产列表
- Agent 设备台账（迁移 `0013`）：`POST /agent/v1/devices` 按 `(tenant_id, fingerprint)` 幂等注册/更新（首次 201，重复 200，同 ID），上报 ECDSA P-256 公钥（base64 PKIX DER，为设备令牌预留）；被运营置为 `disabled` 的设备不会被重新注册激活（403 `device_disabled`）且已存公钥不被覆盖；`POST /agent/v1/heartbeats` 更新 `last_seen_at` 并返回 `server_time`、`agent.min_version` 与特性开关；`GET /agent/v1/ingest-batches/{batchID}` 返回批次、各状态资产计数与 verify job，供 Agent 崩溃后对账；`agent_devices` 启用 `FORCE ROW LEVEL SECURITY`，`pwf_worker` 无任何授权
- finalize 语义：同步封批 + 202 返回 `ingest.verify` job；Worker 回读对象校验大小与 SHA-256 后才置 `verified`，失败标记 `missing`/`corrupt`；重复 finalize 返回同一 job
- 数据库任务队列（`jobs`，`FOR UPDATE SKIP LOCKED` 抢占、指数退避、最大重试、死信）+ 事务性 Outbox（`outbox_events`），独立 `worker` 进程消费
- 幂等中间件：`Idempotency-Key` 命中同 body 重放存储响应，不同 body 返回 409 `idempotency_conflict`，并发返回 409 `idempotency_in_progress`
- 审计日志：项目创建/迁移、批次 finalize 等写操作落 `audit_logs`
- 可观测性：Prometheus 文本 `/metrics`、W3C `traceparent` 透传（响应头 `X-Trace-Id`）、请求超时 504、DB `statement_timeout`
- 统一 `problem+json` 错误、request_id、结构化日志、健康检查
- 内嵌 goose 迁移、种子账号、端到端 smoke 命令
- OpenAPI 契约：`shared/openapi.yaml`

M1 已实现（商务与电子签）：

- 客户档案：创建/查询/修改，版本冲突保护；跨租户 client_id 一律 422
- 线索：状态机（`new → contacted → qualified → converted`，任意非终态可 `lost`）+ 跟进记录 + 转化为客户/项目；转化在同事务内建客户与项目并写审计/Outbox
- 项目简报：按项目一条、`version` 乐观锁，覆盖拍摄需求/地点/交付约定
- 版本化报价：服务端计价（只收 `quantity`/`unit_price_cents`，金额一律服务端算），一项目仅一份草稿；发送即冻结快照（`quote_version` + 明细 + 客户名），并将旧的 sent/accepted 版本标记为 `superseded`；客户中心项目列表展示当前报价，客户可确认，已配置 SMTP 且客户留有邮箱时同时排队邮件通知
- 合同：只能从 sent/accepted 报价生成，快照内嵌报价 vN + 模板版本，生成即写入 `document_hash`（SHA-256）并把总价/定金/尾款同步到项目；同一项目仅一份活跃合同（`active_contract` 409）
- 电子签适配器：`esign.Provider` 接口（`CreateSignTask`/`VerifyCallback`），内置 `sandbox`（HMAC 回调，`X-PWF-Signature: t=<unix>,v1=<hex>`，5 分钟时间窗、常量时间比较）；生产厂商适配器等拿到凭据后再接。工厂对 `esignbao`/`esign`/`docusign` 等已知厂商名直接报“未实现”并拒绝启动，不会静默回退到 sandbox
- 合同发送：先以 `submitting` 预留（5 分钟租约，期间重复请求 409 `send_in_progress`），再调厂商 `CreateSignTask`（契约要求按合同 ID 幂等），落库失败时依次尝试撤销渠道任务、回退 `rendered`；撤销失败则保持 `submitting` 并发 critical 告警 `esign_task_orphaned`（带去渠道任务 ID），超时重试复用同一任务完成发送
- 签署回调：`POST /webhooks/esign/{provider}` 不走 JWT，签名校验 → `esign_tasks` 反查租户与合同 → 租户事务内按合同状态推进（`sent → signing → signed`，未签署可 `void`），事件按 `(provider, event_id)` 去重，重复投递返回 `duplicate` 且不改变任何状态；未知 task 返回 `ignored`。`signed` 事件必须携带 64 位十六进制 `document_sha256` 与非空 `signed_at`，否则按 `malformed_callback` 拒绝，签署文件哈希落 `contracts.signed_document_hash`
- 项目投影：合同创建/发送/签署/作废同步更新 `projects.contract_status`（`none`/`rendered`/`sent`/`signing`/`signed`），**不递增 `projects.version`**；合同签署暂为可选，`booked` 仍由付款状态闸门控制

M2 已实现（支付与账务）：

- 支付订单：`deposit` / `balance` / `full` 三种核心订单，金额一律服务端从已发送报价同步的项目台账推导（签署可选、尾款需定金已付、全款仅适用于未设定金的项目）；变更单签署后自动创建 `change` 补款订单。同一项目同一类型（`change` 按变更单区分）只保留一条 live 订单（`creating`/`pending`），重复创建直接复用，过期或金额变化先关渠道再本地 `closed`（部分唯一索引兜底并发）
- 下单时序：先在事务内落 `creating` 预留行与回调路由，再在事务外调渠道 `CreateOrder`，成功后才发布 `pending` 支付链接；渠道失败会补偿 `CloseOrder` 并本地 `closed`。超过 30 秒未收敛的 `creating` 预留由 Worker 每 60 秒清扫（`payment.SweepCreatingOrders`）：先 `QueryOrder`，渠道无单/已关则本地关单并写审计与 Outbox，渠道已付则保持 `creating` 并发 critical 告警等人工对账；同项目同类型再次下单时也会按同一逻辑收敛
- 支付适配器：`payment.Provider` 接口（`CreateOrder`/`VerifyCallback`/`Refund`/`QueryOrder`/`CloseOrder`），内置 `sandbox`（HMAC 回调、5 分钟时间窗、常量时间比较、商户号校验、进程内订单状态）；`wechatpay`/`alipay` 等已知厂商名启动即报“未实现”，未知名称报未知，绝不回退沙箱；真实适配器等凭据到位后接入
- 支付回调：`POST /webhooks/payment/{provider}` 不走 JWT，验签 → `payment_order_routes` 反查租户与订单 → 租户事务内校验金额/币种 → 写 `payments` 并把订单置 `paid`；`payments UNIQUE (order_id)` + `payment_events UNIQUE (provider,event_id)` 保证重复投递只入账一次；金额/币种不符记 `rejected` 事件与审计后返回 422，绝不入账
- 关单与对账：`POST /payments/orders/{id}/close` 先关渠道再关本地，渠道已收款时改为对账入账并返回 409 `order_already_paid`；`POST /payments/orders/{id}/reconcile` 用 `QueryOrder` 对齐渠道状态，渠道已付按合成事件 `reconcile:<out_trade_no>` 只入账一次，渠道已关则本地关单，状态/金额不符返回 409 `reconcile_mismatch`。复用订单前也会查询渠道，避免“渠道已付、回调丢失”被继续展示为待支付
- 关单后迟到支付：本地已关订单收到验签通过且金额相符的 `payment.succeeded` 时返回 200 `ignored`，事件记 `flagged`，写审计与 Outbox `payment.late_payment` 供人工退款/入账，绝不静默入账
- 退款：按支付单发起，独立 `refunds` 行，不覆盖原支付；未指定金额时退剩余可退额，`pending`+`succeeded` 退款都占用额度，重复退款 409 `refund_exceeds_remaining`；sandbox 异步返回 `pending`，由 `refund.succeeded`/`refund.failed` 回调收敛，失败会记录 `failure_reason` 并释放额度
- 项目投影重算：`payment_status` 由 `net = sum(payments) - sum(succeeded refunds)` 与未过期 pending 订单推导（`unpaid`/`deposit_pending`/`deposit_paid`/`balance_pending`/`change_pending`/`paid`），回调/关单/对账/退款后同事务刷新，**不递增 `projects.version`**；`booked` 闸门要求定金已付、`completed` 要求款清
- 并发控制：所有支付路径统一锁序 project → order → payment → refund；Provider 调用在事务外执行，避免厂商延迟占用项目锁
- 生产守卫：`env=production` 时启动即拒绝 `sandbox` 支付/电子签适配器（含 provider 留空时工厂默认的 sandbox），并拒绝 `dev-only`/`change-me`/`pwf-secret` 等示例密钥与少于 32 字符的 JWT 密钥（`CheckProduction`），防止开发适配器处理真实资金与合同
- 全部写操作写审计与 Outbox（`payment.order_created`/`payment.order_closed`/`payment.order_abandoned`/`payment.succeeded`/`payment.reconciled`/`payment.late_payment`/`payment.refund_requested`/`payment.refunded`/`payment.callback_rejected` 等）

支付安全告警（`internal/alert`）：

- 告警落库 `payment_alerts`，按 `(tenant_id, dedupe_key)` 部分唯一索引去重：同一安全事件重复出现只累加 `occurrences`，不会刷屏；`open → acknowledged → resolved` 三态，重复确认/解决返回 409 `alert_not_open`
- 触发点：回调金额/币种不符（`callback_amount_mismatch`/`callback_currency_mismatch`，critical）、关单后迟到支付（`late_payment`，critical）、渠道已收款仍尝试关单（`close_after_paid`，warning）、对账不一致（`reconcile_mismatch`，warning）、商户号不符（`merchant_mismatch`，critical）；另由发票/变更单模块产生 `invoice_red_flush_required`、`change_order_payment_failed`、`change_order_partially_refunded`、`change_order_paid_after_cancel`
- 通知：告警首次落库时同事务投递 `notify.webhook` job（`jobs_active_subject_uniq` 防重），Worker 读取 `alerts.webhook_*` 配置推送 HMAC 签名请求（`X-PWF-Webhook-Signature: t=<unix>,v1=<hex>`，与签署回调同构；`Idempotency-Key` 恒为告警 ID，重试不变），5xx/超时按 job 退避重试、4xx 直接失败、超过重试上限标记 `dead`；未配置 URL 时 `notify_status=skipped`
- 指标：`/metrics` 输出 `payment_alerts_total{code,severity}` 与 `payment_security_events_total{code}`
- API：`GET /api/v1/payment-alerts`（status/severity/code/project_id 过滤 + 分页）、`GET /{id}`、`POST /{id}/acknowledge`、`POST /{id}/resolve`
- Worker 权限：迁移 `0008` 仅给 `pwf_worker` 授予 `payment_alerts` 的 SELECT/INSERT/UPDATE 与宽松策略用于回写通知状态，其它支付/项目表仍 `permission denied`

发票（`internal/invoice`）：

- 状态机：`requested → approved → issued → sent`；`requested/approved` 可 `void`，`requested` 可 `reject`。审核/驳回/作废是 owner 专属的财务控制，其余操作 owner/staff
- v1 规则：一笔支付最多一张有效发票（`voided/rejected` 不占位），发票金额等于支付金额，不支持部分开票与客户自助申请
- `invoice.Provider` 接口（`Issue`/`RedFlush`）当前只有 `manual`：运营在税务系统开票后回录票号/代码；未知 provider 启动即报错，`manual` 在生产合法
- 文件：`POST /{id}/file-url` 返回 `tenants/{tenant}/invoices/{id}/v{n}.pdf` 的预签名 PUT；`issue` 校验 key 前缀、重新 `Stat` 大小并流式 SHA-256，与提交的 `file_hash` 一致才落库，杜绝伪造哈希
- 红冲：退款成功钩子按 `min(refund, 发票金额 - 已红冲)` 自动生成 `pending` 红冲行（`refund_id` 唯一兜底）并告警；运营上传凭证后 `POST /invoices/{id}/red-flushes/{rid}/issue` 登记；`issued/sent` 发票不可作废，只能红冲
- 所有迁移带 `expected_version` CAS，写审计与 Outbox（`invoice.*`）

变更单（`internal/changeorder`）：

- 业务规则：合同签署后才能发起；行项目合计即变更金额（服务端计算）；同一项目同时只允许一张活跃变更单（`draft/awaiting_signature/signed/awaiting_payment`）
- 流程：`draft → awaiting_signature → signed → awaiting_payment → applied`。发送走电子签渠道；签署回调落 `signed` 后自动创建 `kind=change` 补款单（失败不回滚签署，暴露 `POST /{id}/payment-order` 幂等重试接口）；补款支付成功后在支付结算钩子内置 `applied` 并重算 `projects.change_amount_cents`
- 取消：`applied/reversed` 之外均可取消；有 live 补款单时先关渠道再本地关单，渠道已收款返回 409 `change_order_paid`，迟到支付仍入账并告警 `change_order_paid_after_cancel`
- 退款：补款全额退款 → `applied → reversed` 并重算投影；部分退款保持 `applied` 并发 `change_order_partially_refunded` 告警
- 电子签回调分发：`esign_tasks` 扩展为多态（`contract_id`/`change_order_id` 二选一），`internal/signing` 验签一次后按任务归属分发到合同或变更单聚合；响应新增 `subject_type`/`subject_id`
- 金额口径：变更应收独立投影到 `projects.change_amount_cents`，不污染合同快照 `total_amount_cents`/`balance_cents`；有效总额 = 合同总额 + 变更总额，`balance` 订单金额仍只含核心应收
- 已知残余：变更单发送尚未采用 `submitting` 预留 + 撤销补偿（合同已落地），真实签署厂商接入时一并补齐

M3 已实现（排期核心）：

- 资源档案：`photographer`/`studio`/`makeup`/`assistant`/`equipment` 五类，容量恒为 1；每资源可配置前置/后置缓冲（0–1440 分钟），缓冲只扩大占用区间、不放宽可用性规则
- 周可用性：`availability_rules` 按 ISO 星期（1=周一…7=周日）+ 本地分钟区间 `[start_minute, end_minute)`，按 `PWF_SCHEDULE__TIMEZONE` 解释；无规则 = 7×24；预约覆盖的每个本地日都必须被当日规则完整覆盖，否则 422 `outside_availability`；最长 7 天（按本地墙钟计，DST 切换不改变天数）
- 锁位：创建预约即写 `held` + `hold_expires_at`（`PWF_SCHEDULE__HOLD_TTL`，默认 30m），并为每个资源写 `kind=hold` 的 `resource_blocks`；锁位只要求项目存在且非终态，允许未签约先锁档，确认时才校验合同与款项
- 防撞档：`resource_blocks` 上的 GiST 排他约束 `EXCLUDE (tenant_id WITH =, resource_id WITH =, during WITH &&)` 是唯一仲裁者（SQLSTATE 23P01 → 409 `slot_taken`）；应用层预检只负责返回冲突资源名，不承担正确性
- 确认：要求定金/全款已付，合同签署可选。定金或全款支付成功时，`payment.OrderHook` 在同一事务内自动确认该项目未过期的 held 预约（blocks 改 `kind=booking`、清 `expires_at`）并同步 `projects.shoot_start_at/end_at`；锁位已过期或项目已取消等无法确认的情况则不确认、保留款项并触发 critical 告警（`booking_hold_expired` / `booking_confirm_blocked`）供人工退款，绝不因预约问题回滚已入账的支付。手动 `POST /bookings/{id}/confirm` 与钩子复用同一路径，已确认再确认是成功 no-op
- 改期/取消：`held`/`confirmed` 均可改期（同一事务删旧块、插新块，仍由排他约束仲裁）与取消（释放全部占位、项目进入 `cancelled`；confirmed 且项目时间与预约一致时清空项目拍摄时间）；客户中心显示项目已取消并禁止接受历史报价/新建收款单。取消**不自动退款**，退款走支付接口
- 过期回收：Worker 每轮以 `pwf_worker` 身份 `FOR UPDATE SKIP LOCKED` 扫描 `held` 且已过期的预约，删除占位、置 `expired` 并 Emit `booking.expired`
- 唯一性：部分唯一索引保证每个项目同时只有一条 `held`/`confirmed` 预约，项目拍摄时间与预约一一对应
- API：`POST/GET /resources`、`GET/PATCH /resources/{id}`、`PUT/GET /resources/{id}/availability`、`POST/GET /resources/{id}/blocks`、`DELETE /resources/{id}/blocks/{blockID}`、`GET /schedule/availability`、`POST/GET /bookings`、`GET /bookings/{id}`、`POST /bookings/{id}/confirm|reschedule|cancel`

M3 拍前准备与自动提醒（`internal/prep` + `internal/notice`）：

- **场次 = 预约**：不新增 `shoots` 表，shot list、器材清单、授权台账与 call sheet 全部以 `booking_id` 归属；改期自动跟随新时间，取消/过期后全部只读（历史可查），写接口返回 409 `illegal_state`
- shot list：每个 booking 一份头（重复创建 409 `shot_list_exists`），条目独立版本化（`category/title/description/reference_url/priority/done`），`reference_url` 仅允许 http(s)，`priority` 为 `low/normal/high`；追加条目自动取 `sort_order = max+1`，现场勾选 `done` 由服务端写 `done_at`
- 器材清单：目录沿用 `resources(kind='equipment')`，`shoot_gear` 只记场次清单、数量（1–100）与勾选状态；可关联器材资源（`resource_id` 跨租户/非器材一律 422），同名条目 409 `gear_conflict`
- 授权台账：`model_release`/`venue_permit`/`usage_license`/`other`，对象为模特/场地方/被授权方；同一 booking 内 `(kind, subject)` 唯一；状态 `pending/signed/waived`，`signed` 的 `signed_at` 一律服务端时间（不接受客户端回填），`waived` 必须写备注，`signed/waived` 是证据不允许删除（409 `illegal_state`），只有 `pending` 可删
- call sheet：仅 `confirmed` 预约可冻结（held 返回 409），每次冻结生成新版本；快照为 Go 结构体序列化的规范 JSON（空集合是数组、无 map 字段、UTC 时间），以 `text` 原样存储并记录精确字节的 SHA-256；GET 时重新计算哈希，不一致返回 500 而不是服务未校验数据；改期不回溯旧版本，需要新通告就再冻结 v2，v1 永远保留原时间与哈希
- call sheet HTML：服务端 `html/template` 从冻结快照按需渲染，自动转义 + URL 限 http(s)，响应带 `Cache-Control: no-store`、`X-Content-Type-Options: nosniff`、CSP `default-src 'none'; style-src 'unsafe-inline'; img-src data:; base-uri 'none'; form-action 'none'`；无 JS、无外部资源，不引入 PDF/浏览器运行时
- 自动提醒：`notifications` + `notification_deliveries` 落库，三类提醒（拍摄前 `shoot_reminder`、锁位到期 `hold_expiry`、拍摄前仍有未完成授权 `auth_incomplete`）在创建锁位/确认（含支付钩子）/改期/取消/授权变更时同事务重排；规划包在 savepoint 中，失败只记日志，**绝不回滚预约或已入账支付**
- 去重与状态：去重键含 booking 与时间（`hold:<id>:<expiresUnix>`、`shoot:<id>:<startsAtUnix>:<lead>`、`auth:<id>:<startsAtUnix>`），仅 pending 行占用唯一键；改期产生新键并取消旧 pending，已 `sent` 的键永不重发；Worker 以 `notice.deliver` job 扫描到期行，投递前复核 booking 状态/时间与 pending 授权数，过期/已取消/已改期/授权已完成的一律 `skipped`
- Webhook 投递：`X-PWF-Event: notification`、`X-PWF-Notification-Type`、`X-PWF-Webhook-Signature: t=<unix>,v1=<hex>`，与告警/回调共用 `internal/webhook` 的 HMAC 实现；`Idempotency-Key` 恒为站内通知 ID，接收方按它去重（Worker 租约丢失后的重投是 at-least-once）；4xx 永久失败记 `failed`，5xx/超时按 job 退避重试，5 次后 `dead`；未配置 Webhook 时站内通知仍可读，投递记 `skipped`
- API：`GET/POST /bookings/{id}/shot-list`、`PATCH /shot-lists/{id}`、`POST /shot-lists/{id}/items`、`PATCH/DELETE /shot-items/{id}`、`GET/POST /bookings/{id}/gear`、`PATCH/DELETE /gear/{id}`、`GET/POST /bookings/{id}/authorizations`、`PATCH/DELETE /authorizations/{id}`、`POST/GET /bookings/{id}/call-sheets`、`GET /call-sheets/{id}`、`GET /call-sheets/{id}/html`、`GET /notifications`、`GET /notifications/{id}`、`POST /notifications/{id}/read`
- 权限：拍前准备读写限 `owner/staff/assistant`（call sheet 含客户联系方式，不向 retoucher 开放），通知读取 `owner/staff/retoucher/assistant`；所有写接口走 `Idempotency-Key`，删除用 `?expected_version=`

运行时加固（2026-10-31 代码评审修复，迁移 `0014_runtime_hardening.sql`）：

- **Outbox 租约**：`outbox_events` 增加 `locked_by`/`lease_token`，`available_at` 兼作租约截止；领取、成功、失败更新都匹配当前租约，超时被其它 Worker 回收后旧持有者不能再次确认投递。投递语义仍是 at-least-once，但不会出现“已经换人还替别人签收”
- **任务终态租约**：`jobs` 的成功/失败更新都匹配 `locked_by + lease_token`；租约已被回收时不写终态，也不把告警或提醒标记 `dead`，由持有新租约的 Worker 决定最终结果
- **合同发送 `submitting` + 补偿**：见 M1 条目；`esign.TaskCanceller` 为可选能力，sandbox 已实现幂等建单与撤销，真实厂商按同一契约接入。作废已发送合同时先提交本地 `void` 再撤销渠道任务：撤销失败不回滚 `void`，而是发 critical 告警 `esign_task_revoke_failed`，避免出现“合同仍为 sent 但签署链接已失效”的错位
- **`creating` 订单后台清扫**：Worker 每分钟扫描超过 30 秒的 `creating` 预留，按渠道状态关单或告警；Worker 角色新增 `payment_orders` 读写与 `audit_logs` 插入授权（含序列），其它业务表仍不可读
- **过期锁位失败关闭**：提醒取消失败会让整个过期事务回滚，下一轮重新扫描；不会出现“锁位已过期、提醒还在排队”
- **Provider 工厂 fail-fast**：`wechatpay`/`alipay`/`esignbao` 等已知厂商名启动即报“未实现”，未知名称报未知；生产守卫同时拒绝 sandbox（含留空默认）与示例密钥，配置错误不可能静默跑在沙箱上

M0–M7 已交付：M4 的 UNC/真实拔卡/磁盘满属现场浸泡项（步骤见“Windows Agent（M4）”章节），M5 的图库与选片见“图库与选片（M5）”，M6 的审片与后期集成见“后期集成与审片（M6）”，M7 的交付下载与归档见“交付下载与归档（M7）”。上线验证工具（压测 CLI、支付重放）与演练手册见 `server/cmd/`、`scripts/` 与 `docs/`。

## 目录结构

```
server/
  cmd/api       HTTP 服务
  cmd/worker    异步任务 Worker（ingest.verify、outbox 投递、交付构建、归档校验、保留告警）
  cmd/migrate   迁移（up/down/status/version）
  cmd/seed      租户 + owner 账号
  cmd/smoke     M0–M7 端到端验证
  cmd/mailsink  开发用 SMTP 收件箱（.eml 落盘，E2E/smoke 用）
  cmd/loadtest  压测 CLI（选片写 / 普通 API / 200 并发抢档，带 p95 与错误率阈值）
  cmd/paymentreplay 支付回调与退款重放工具（仅测试渠道，100 次顺序 + 100 次并发）
  config/       config.yaml（可被 PWF_* 环境变量覆盖）
  db/           内嵌迁移 SQL
  internal/     archive/auth/alert/audit/brief/changeorder/client/config/contract/db/delivery/device/esign/gallery/httpx/idempotency/ingest/integration/invoice/jobs/lead/media/notice/outbox/payment/prep/project/proof/quote/schedule/signing/storage/webhook/worker
shared/openapi.yaml
deploy/docker-compose.yml   postgres16 / redis7 / minio / gotenberg
docs/runbook-delivery-archive.md  交付/归档故障处置手册
docs/dr.md                 备份、PITR 与灾备演练手册
scripts/ci.sh               Docker 集成门禁（等同于 make ci）
scripts/agent-e2e.sh        Agent 无头导入 E2E（双写/去重/selftest/派生上传）
scripts/gallery-e2e.sh      M5+M6+M7 独立栈 + Playwright 管理端/客户全流程（含交付下载）
scripts/migration-matrix.sh 迁移 up/down/up 与 0019/0018/0017/0015 数据保护验证
scripts/loadtest.sh         压测入口（包装 cmd/loadtest）
scripts/payment-replay.sh   支付重放入口（包装 cmd/paymentreplay）
scripts/launch-drill.sh     一条命令的隔离栈上线演练（重放 + 压测，报告写入 deploy/.drill）
web-admin/                  官网、认证与管理端（Vue 3 + TS + Vite，图库 + 线索 + 审片 + 交付/归档模块）
web-gallery/                客户 H5（Vue 3 + TS + Vite，hash 路由 + 选片/审片/交付链接）
web-client-miniapp/         客户中心（uni-app Vue 3 + TS，小程序预约 + H5 本地预览）
Lumeo 官网视图与样式由 web-admin 根路由提供，与登录、注册和管理端共用同一前端端口；未登录访问管理路由不会被强制改送登录页
.github/workflows/ci.yml    unit + agent-unit + web + integration
.github/workflows/agent-windows.yml  Windows 单测/GUI 构建/无头 E2E/NSIS 打包
agent/
  main.go                 Wails GUI（//go:build wails）
  cmd/pwf-agent           无头 CLI（import/resume/status/login/upload/update-check/selftest）
  cmd/pwf-launcher        读取 current.txt 启动版本目录
  cmd/pwf-release         Ed25519 清单签名与安装包 Authenticode 校验
  internal/               binder/store/core/scan/copy/hash/meta/preview/api/auth/card/remote/upload/updater/…
  frontend/               Vue 3 + TS + Vite
  build/installer.nsi     NSIS 每用户安装包
  build/package.sh        发布流水线（前端 → wails → CLI/launcher → NSIS → 签名 → 清单）
```

## 快速开始

最省事的方式：安装 Docker Desktop 后，在项目目录执行：

```bash
docker compose -f deploy/docker-compose.yml up --build
```

首次启动会构建并启动 API、后台 Worker、Postgres、MinIO、管理端、客户账号中心与分享图库；数据库迁移和演示账号会自动初始化。打开：

- 管理端（含官网）：<http://localhost:5175>，登录 `owner@example.com` / `photographer123`
- 客户账号中心（登录、摄影师编号预约、我的拍摄）：<http://localhost:5173>
- 客户图库（选片/审片/交付分享链接）：<http://localhost:5174>
- API 健康检查：<http://localhost:8080/healthz>
- MinIO 控制台：<http://localhost:9001>，登录 `pwf` / `pwf-secret`

采集 Agent 的服务地址填写 `http://localhost:5175`，以便通过管理端代理访问 API 和对象存储。

停止服务但保留数据：`docker compose -f deploy/docker-compose.yml down`。要清空本地数据库和图片，再执行 `docker compose -f deploy/docker-compose.yml down -v`。

### 本机开发

前置：Go 1.26+、Node.js 22+、Docker Desktop。以下步骤适合需要热更新或调试 Go 代码时使用。

```bash
# 1. 起依赖
make up

# 2. 建表
make migrate

# 3. 创建演示租户与账号（owner@example.com / photographer123）
make seed

# 4. 启动 API（:8080）
make api

# 5. 另开终端启动 Worker（异步校验 finalize 投递的对象、投递告警与提醒 Webhook、回收过期锁位）
make worker

# 6. 再开终端跑端到端验证
make smoke            # M1 商务/签署 + M2 支付/退款/发票/告警/变更单 + M3 资源/锁位/确认/改期/过期/并发 + 拍前准备/提醒 + 单次 PUT + 去重 + 500 条批量 + M5 图库/选片 + M6 审片/批注/修图/闸门/邮件 + M7 交付构建/下载/撤销/退款闸门/归档核验全流程
make smoke-multipart  # 40MiB 文件，走 16MiB 分片上传

# 7. 一条命令跑完整 CI（独立端口与 compose 项目，不影响本地开发栈）
make ci

# 8. 前端：另开三个终端
cd web-admin && npm ci && npm run dev      # http://localhost:5175（首页为 Lumeo 官网；/#/login、/#/register 进入认证页）
cd web-gallery && npm ci && npm run dev    # http://localhost:5174/#/a/<分享 token> 或 /#/p/<审片 token>
cd web-client-miniapp && npm ci && npm run dev:h5 # http://localhost:5173，预览客户预约；本地模拟登录需 PWF_CUSTOMER__DEV_LOGIN=true
```

客户 H5 开发代理默认将 `/c/` 转发到 `http://localhost:8080`。若 API 使用其他端口，启动 H5 前设置 `VITE_API_PROXY_TARGET` 为同一套 API 地址（例如 `http://localhost:18080`）；同时将后端 `PWF_CUSTOMER__PUBLIC_BASE_URL` 设为 H5 实际地址。H5 固定使用 5173 端口；端口被占用时会明确报错，不再静默换端口并生成指向旧端口的邀请。静态部署时将 `VITE_API_BASE` 设为该 API 地址，或由同源反向代理转发 `/c/`。预约邀请 token 绑定其创建时所在的数据库，H5 必须请求同一套后端；重置数据库或使用过期邀请后需重新生成链接。

摄影师在管理端「预约」中维护套餐和可用时段，并生成客户邀请链接/小程序码。客户扫码进入小程序后登录即绑定工作室；客户可在“我的拍摄”中粘贴原有 `/#/a`、`/#/p`、`/#/d` 分享链接打开选片、审片或交付内容。生产小程序登录及官方小程序码生成需要配置 `PWF_CUSTOMER__WECHAT_APP_ID` / `PWF_CUSTOMER__WECHAT_APP_SECRET`；在 `web-client-miniapp/src/manifest.json` 中填写微信小程序 AppID，并在构建时设置 `VITE_GALLERY_BASE_URL` 为 `web-gallery` 的 HTTPS 域名。发布前还需在微信公众平台配置 request/web-view 合法域名。

本地想验证客户邮件，可另起一个 SMTP 收件箱，并把 `PWF_NOTIFY__EMAIL__*` 指过去：

```bash
go run ./server/cmd/mailsink -addr 127.0.0.1:2525 -dir ./tmp/mail
export PWF_NOTIFY__EMAIL__ENABLED=true PWF_NOTIFY__EMAIL__HOST=127.0.0.1 \
       PWF_NOTIFY__EMAIL__PORT=2525 PWF_NOTIFY__EMAIL__FROM=proof@example.com PWF_NOTIFY__EMAIL__TLS=none
# 每封邮件以 .eml 落盘（保留 X-MailSink-From/To 头，正文为 base64）
```

`web-admin`、`web-gallery` 和客户 H5 预览分别将 `/api/`、`/g/`、`/c/` 代理到 `:8080`；官网、摄影师认证和工作台由 `web-admin` 同一构建产物提供。生产构建是静态文件，托管在同一站点（或设置 `VITE_API_BASE` 指向 API 源）。管理端若与 API 跨域部署，API 还需配置 `PWF_HTTP__CORS_ORIGINS` 为管理端的精确 origin 列表。管理端发布相册后生成 `http://localhost:5174/#/a/<token>` 形式的分享链接，发起审片后生成 `http://localhost:5174/#/p/<token>` 形式的分享链接。

`make smoke` 会真实执行：

```
健康检查 + /metrics 断言 → 登录 → 401 断言 → 建项目（重复 code=409、跨租户 client=422、时间倒挂=422）
→ 幂等断言：同 Idempotency-Key 同 body 重放同一项目、不同 body=409 idempotency_conflict
→ 状态机：缺 expected_version=422、过期版本=409 version_conflict、非法跳转=409、draft→quoted→contract_pending→contracted 无签署也可通过
→ M1：建客户 → 建线索 + 跟进 → 转化（复用客户并建项目）→ 简报写入/更新/过期版本=409
→ 报价 v1 计价并发送（快照冻结）→ 由快照生成合同（document_hash、项目金额同步、重复=409 active_contract）
→ 发送合同（sandbox 签署任务）→ 回调 signing→signed、重复回调版本不变、篡改报文=401 invalid_signature、缺 document_sha256=400 malformed_callback、未知 task=ignored
→ 项目 contract_status=signed 后 draft→quoted→contract_pending→contracted 通过；再次发送新报价=409 active_contract
→ M2：未签署项目发送报价后可建定金单（金额来自冻结报价）→ 定金单创建/重复创建复用 → 金额篡改回调=422 amount_mismatch 且不入账
→ 签名篡改=401、未知订单=ignored、定金回调入账后重复事件/新事件均只入账一次
→ 退款→pending→refund.succeeded 回调后支付行保留、项目回 unpaid、重复退款=409、重复回调=duplicate
→ 重新付定金→booked；退款失败回调→failed 且额度释放
→ 尾款单关闭 → 迟到支付回调=ignored（仅告警）→ 对账已关订单=409 reconcile_mismatch → 重开尾款单 → 被拒回调后对账入账一次 → 重复对账不重复入账 → paid→completed
→ 已全额退款的支付再退款=409 refund_exceeds_remaining
→ 发票：申请（金额镜像支付、重复=409 active_invoice、提前发送/上传=409）→ 审核 → 预签名上传 PDF → issue（哈希校验）→ send → 重复 issue=409
→ 发票号复用=409 invoice_no_conflict → 已审核发票作废 → 退款生成红冲待办（重复回调不重复生成）→ 红冲凭证上传并登记 → 已开票不可作废
→ 告警：金额不符/迟到支付/关单后支付/对账不一致落库（6 条）+ 商户号不符计数 → 确认 → 解决 → 重复操作=409 alert_not_open
→ 告警 Webhook：本地接收器验 HMAC 签名，断言 notify_status=sent、attempts=1
→ M3：建摄影师/影棚资源 + 每日 09:00–18:00 规则 → 可用性查询 → 已签约项目 + 定金单 → 锁位 2 资源
→ 定金回调自动确认（项目拍摄时间同步、重复确认 no-op）→ 重叠锁位/手工屏蔽=409 slot_taken → 规则外 08:00=422 outside_availability
→ 改期：旧档释放、新档占用、项目时间更新 → 取消 confirmed：资源释放、项目时间清空、重复取消=409
→ 5s TTL 锁位创建后即有 pending `hold_expiry` 提醒（due 不晚于 hold_expires_at），随后由 Worker 回收为 expired，档期可再次锁位并释放
→ 并发验收：200 个草稿项目并发抢同一档期，恰好 1 个成功、199 个 409 slot_taken（数据库排他约束仲裁）
→ 拍前准备：无规则资源 + 已签约定金项目 → 锁位（held 冻结 call sheet=409 illegal_state）→ 定金回调确认（hold 提醒取消）
→ shot list 创建（重复=409、javascript: URL=422、非法 priority=422）→ 3 条条目（含 `<script>` 标题）→ 勾选 done/过期版本=409 → 删除一条
→ 器材：数量 101=422、非器材资源=422 → 关联器材资源新增（名称取资源名）→ 重名=409 → 勾选核对
→ 授权：非法 kind=422 → 新增 pending 肖像授权（重复=409）→ 断言 shoot_reminder/auth_incomplete 已规划
→ call sheet v1：version=1、SHA-256 64 位且与重算一致、快照含 booking/shot/gear/auth → HTML 断言 text/html、安全响应头、`&lt;script&gt;` 转义、无原始脚本标签、版本/哈希页脚
→ 提醒（CI，`-reminder-lead 1m`）：等 shoot_reminder/auth_incomplete 变为 sent，断言 delivery webhook sent、接收器验 HMAC、标记已读后 unread 下降（本地无 lead 时跳过等待，只验证规划）
→ 授权 signed（服务端写 signed_at）→ 删除 signed=409 → 无 pending auth_incomplete
→ 改期 +2h：新 pending shoot_reminder 按新 startsAt 计算 → 冻结 v2：哈希与 v1 不同、v1 仍返回原时间与哈希
→ 取消：pending 提醒全部 cancelled、v1/v2 仍可读、拍前准备写入全部 409 illegal_state
→ 变更单：建草稿（2 行合计 70000、重复=409 active_change_order、绕过接口建 kind=change=422）→ 发送电子签 → signed 回调（自动建补款单、重复回调幂等、伪造签名=401）
→ 补款支付入账 → applied 且项目 change_amount_cents=70000（已付变更单取消=409 change_order_paid）
→ 部分退款→warning 告警且保持 applied → 全额退款→reversed、投影回到 0
→ 取消路径：live 补款单先关渠道再取消、迟到支付=ignored、重复取消=409 illegal_state
→ 注册 Agent 设备（首次 201、重复 200 且同 ID、未认证 401）→ 心跳（server_time/特性开关）→ 未知设备心跳 404
→ 开采集批次 → 批量登记（sha256 内容哈希，返回资产状态）→ 重复登记断言幂等
→ 预签名 → PUT 到 MinIO → complete 校验对象大小 → finalize 返回 202 job → 轮询 job 至 succeeded 并断言 verified=1
→ 批次状态对账（completed、1 verified、verify job 与 finalize 同 ID）→ 未知批次 404
→ 再 finalize 返回同一 job → 关闭批次拒绝登记=409 → 列出资产
→ 已 verified 资产拒绝再次预签名=409
→ 批量登记（默认 500 条，含耗时）→ 未预签名 complete=409、错误大小=422
→ M5：建项目并上传 3 张 800×600 JPEG → 建相册（上限 1）并加入资产 → 缺派生时发布=409 derivatives_incomplete
→ 补齐派生（thumb/display/LQIP）→ 发布并冻结 manifest（重发同 key 幂等）→ 发分享 token → 客户会话 → manifest 分页 + 签名 URL 实取字节
→ 选片批量写为 v1、同 Idempotency-Key 重放结果不变、旧版本=409 version_conflict（带 current_version）、超上限=409 selection_limit_exceeded
→ 提交（客户视图 submitted_at、员工结果视图计数、`gallery_submitted` 通知）→ 撤销 token 后旧会话立即 401 → reopen 后可继续修改 → 撤销相册后客户 410
```

成功后输出 `M0+M1+M2+M3+M4+M5 loop OK: auth -> project -> commerce/signature -> payments/refunds -> invoices/alerts -> scheduling/holds/expiry/race -> prep/call-sheet/reminders -> change orders -> agent devices/heartbeat -> ingest -> presigned upload -> finalize -> batch status -> list -> gallery publish/token/session/selection/submit/revoke`。回调签名默认使用开发密钥，可用 `-esign-secret` / `-payment-secret` 覆盖（需分别与 `PWF_ESIGN__CALLBACK_SECRET` / `PWF_PAYMENT__CALLBACK_SECRET` 一致），`-payment-merchant` 需与 `PWF_PAYMENT__MERCHANT_ID` 一致。告警与提醒共用同一接收器：`-alert-listener 127.0.0.1:18081 -alert-secret ... -reminder-secret ...` 启用（需与 `PWF_ALERTS__*` / `PWF_REMINDERS__WEBHOOK_*` 对应，接收器按 `event` 选择对应密钥验签）；`-job-timeout` 控制 finalize 轮询上限。排期相关：`-schedule-timezone` 需与 `PWF_SCHEDULE__TIMEZONE` 一致，`-schedule-hold-ttl` 需与 `PWF_SCHEDULE__HOLD_TTL` 一致（≤30s 时 smoke 才执行 Worker 过期回收步骤），`-booking-race` 控制并发抢档的项目数（本地默认 20，CI 设 200）。提醒相关：`-reminder-lead` 需与 `PWF_REMINDERS__SHOOT_LEADS` 一致，>0 时 smoke 等待 Webhook 投递（CI 设 `1m`，预约开始取 `now + lead + 20s`，最多等 60s）；不设置时跳过定时投递断言，仅验证规划/改期重排/取消。`-reminder-secret` 需与 `PWF_REMINDERS__WEBHOOK_SECRET` 一致。重复运行会生成新项目与新批次；同一文件内容在同一项目内只会保留一条资产（`inserted=0 duplicated=1`）。`-bulk 0` 可跳过批量检查，`-size 41943040` 会走分片上传并断言分片会话复用。smoke 需要 Worker 在运行，否则 finalize 后的校验 job、告警/提醒 Webhook 与过期锁位回收不会完成。

### 端口说明

`deploy/docker-compose.yml` 里 Redis 映射到宿主机 `6380`、Gotenberg 映射到 `3800`：本机 6379 已被占用，且 3000 落在 Windows 保留端口段内。

MinIO 在 2026 年 9 月下架了 Docker Hub 镜像，本地 compose 改用社区镜像 `pgsty/minio`；生产的对象存储是 OSS/COS，只有本地开发依赖它。

### 数据库角色与 RLS

- `pwf`（owner）：跑迁移与 seed，`make migrate` / `make seed` 使用 `database.migrate_url`。
- `pwf_app`（非超级用户）：API 使用 `database.url`，因此 RLS 真正生效。
- `pwf_worker`（非超级用户、`NOBYPASSRLS`）：Worker 使用独立的 `database.worker_url`，只被授予 `jobs`/`assets`/`ingest_batches`/`outbox_events`/`payment_alerts` 以及排期的 `bookings`/`resource_blocks` 的 DML；迁移 `0007` 撤销了它在其馀表的权限与默认权限，`0008` 单独授予 `payment_alerts` 的 SELECT/INSERT/UPDATE（回写通知状态），`0011` 授予 `bookings` 的 SELECT/UPDATE 与 `resource_blocks` 的 SELECT/DELETE（回收过期锁位），`0012` 授予 `notifications` 的 SELECT/UPDATE、`notification_deliveries` 的 SELECT/INSERT/UPDATE 与 `booking_authorizations` 的 SELECT（投递前复核 pending 授权），并为这些表加上仅限该角色的宽松策略（`TO pwf_worker USING (true)`），因此既能跨租户消费任务，也读不到支付、合同、客户等业务数据。`0013` 的 `agent_devices` 不向 Worker 授予任何权限；`0015` 授予 `galleries`/`gallery_assets`/`gallery_tokens`/`asset_derivatives` 的读写与 `notifications` 的 INSERT（过期清扫、截止提醒与派生登记），`gallery_tokens` 只 ENABLE、不 FORCE RLS，客户会话交换通过 `SECURITY DEFINER` 函数完成哈希反查。

已在本地库验证：不设置租户时 `select count(*) from projects` 返回 0；设置 `app.tenant_id` 后只看到本租户数据；跨租户 insert 直接报 `row-level security policy`。以 `pwf_worker` 连接时 `jobs`/`assets`/`outbox_events`/`payment_alerts`、排期的 `bookings`/`resource_blocks` 与提醒的 `notifications`/`notification_deliveries`/`booking_authorizations` 可读写（RLS 策略放行），而 `payment_orders`/`projects`/`resources`/`availability_rules`/`shot_lists`/`shot_items`/`shoot_gear`/`call_sheets`/`agent_devices` 等表直接返回 `permission denied`。

例外：`users`/`memberships`/`refresh_tokens` 暂未启用 RLS。登录发生在选定租户之前，只能按邮箱跨租户查用户；成员关系在事务内显式按 `user_id` 校验。业务表（projects/assets/ingest_batches/jobs/outbox_events/idempotency_keys/audit_logs 等）全部启用 RLS 且 `FORCE`。

跨租户访问统一返回 404（不泄露资源是否存在）。

已有数据卷不会重跑 `deploy/postgres/init/01-app-role.sql`，如角色缺失执行 `docker compose down -v` 重置（会清空本地数据）。

早期数据卷可能存在“迁移版本已记录、但列/约束缺失”的漂移：迁移 `0005_reconcile_m0_drift.sql` 用 `ADD COLUMN IF NOT EXISTS` / 重建约束的方式幂等补齐（`idempotency_keys.response_*`、`jobs.lease_token`、`ingest_batches` 状态枚举），老库直接 `make migrate` 即可对齐。

## 手工 curl 演示

```bash
BASE=http://localhost:8080

TOKEN=$(curl -s $BASE/api/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"email":"owner@example.com","password":"photographer123"}' \
  | sed -n 's/.*"access_token":"\([^"]*\)".*/\1/p')

curl -s $BASE/api/v1/projects \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"title":"王先生婚礼","total_amount_cents":3000000,"deposit_cents":900000}'

curl -s "$BASE/api/v1/projects?state=draft&limit=20" \
  -H "Authorization: Bearer $TOKEN"
```

状态迁移（必须按状态机走，非法迁移返回 409，版本不匹配同样返回 409 但 code 为 `version_conflict`）：

```bash
curl -s -X POST $BASE/api/v1/projects/<id>/transitions \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"to":"quoted","expected_version":1,"reason":"报价已发送"}'
```

M1 商务链路（报价 → 合同 → sandbox 回调）：

```bash
# 客户与线索
CLIENT=$(curl -s $BASE/api/v1/clients -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"name":"王先生","phone":"13800000000"}' | jq -r .id)
LEAD=$(curl -s $BASE/api/v1/leads -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"name":"王先生婚礼","source":"referral"}' | jq -r .id)
curl -s -X POST $BASE/api/v1/leads/$LEAD/follow-ups -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"channel":"phone","summary":"确认档期与预算"}'

# 报价（金额由服务端计算；一个项目只允许一份草稿）
QUOTE=$(curl -s $BASE/api/v1/quotes -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d "{\"project_id\":\"<projectID>\",\"items\":[{\"kind\":\"service\",\"description\":\"婚礼跟拍\",\"quantity\":1,\"unit_price_cents\":250000}]}" | jq -r .id)
curl -s -X POST $BASE/api/v1/quotes/$QUOTE/send -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"expected_version":1}'

# 合同（只能从已发送报价生成，快照冻结）
CONTRACT=$(curl -s $BASE/api/v1/contracts -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d "{\"project_id\":\"<projectID>\",\"quote_id\":\"$QUOTE\",\"signer_name\":\"王先生\",\"signer_contact\":\"13800000000\"}" | jq -r .id)
curl -s -X POST $BASE/api/v1/contracts/$CONTRACT/send -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"expected_version":1}'
```

sandbox 签署回调不走 JWT，需按 `X-PWF-Signature: t=<unix>,v1=hex(hmac_sha256(secret, "<t>.<body>"))` 签名 `POST /webhooks/esign/sandbox`；`make smoke` 已覆盖 signing/signed/重复事件/篡改报文/未知 task 五种情况。

M2 支付链路（订单 → 回调 → 退款）：

```bash
# 定金订单：金额由服务端推导；重复调用返回同一 pending 订单（200 而非 201）
curl -s $BASE/api/v1/payments/orders -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d "{\"project_id\":\"<projectID>\",\"kind\":\"deposit\"}"

# 支付回调与电子签同一签名规则，路径为 /webhooks/payment/sandbox
# body 关键字段：event_id / type=payment.succeeded / merchant_id / out_trade_no / amount_cents / currency

# 退款（省略 amount_cents 表示退剩余可退金额；sandbox 先 pending，再由回调收敛）
curl -s $BASE/api/v1/payments/<paymentID>/refunds -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"reason":"客户取消定金"}'

# 关单（先关渠道再关本地；渠道已收款时 409 order_already_paid）与对账（回调丢失时的恢复入口）
curl -s -X POST $BASE/api/v1/payments/orders/<orderID>/close -H "Authorization: Bearer $TOKEN"
curl -s -X POST $BASE/api/v1/payments/orders/<orderID>/reconcile -H "Authorization: Bearer $TOKEN"
```

M2 发票 / 变更单 / 告警（均走后台 JWT；审核类动作仅 owner）：

```bash
# 发票：申请（金额取自支付）→ owner 审核 → 预签名上传 → 登记票号 → 发送
INVOICE=$(curl -s $BASE/api/v1/invoices -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"payment_id":"<paymentID>","title":"王先生","type":"normal"}' | jq -r .id)
curl -s -X POST $BASE/api/v1/invoices/$INVOICE/approve -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"expected_version":1}'
curl -s -X POST $BASE/api/v1/invoices/$INVOICE/file-url -H "Authorization: Bearer $TOKEN"
# 上传 PDF 到返回的 url 后：
curl -s -X POST $BASE/api/v1/invoices/$INVOICE/issue -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"expected_version":2,"invoice_no":"12345678","file_key":"<object_key>","file_hash":"<sha256>"}'
curl -s -X POST $BASE/api/v1/invoices/$INVOICE/send -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"expected_version":3}'

# 变更单：建草稿 → 发送电子签 → 签署回调后带 payment_order_id → 支付入账自动 applied
CHANGE=$(curl -s $BASE/api/v1/change-orders -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"project_id":"<projectID>","reason":"额外精修 20 张","signer_name":"王先生","items":[{"kind":"extra_retouch","description":"精修 20 张","quantity":20,"unit_price_cents":3500}]}' | jq -r .id)
curl -s -X POST $BASE/api/v1/change-orders/$CHANGE/send -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"expected_version":1}'
# 若签署回调时自动建单失败，可幂等重试：
curl -s -X POST $BASE/api/v1/change-orders/$CHANGE/payment-order -H "Authorization: Bearer $TOKEN"

# 告警：列表 / 确认 / 解决
curl -s "$BASE/api/v1/payment-alerts?status=open&severity=critical" -H "Authorization: Bearer $TOKEN"
curl -s -X POST $BASE/api/v1/payment-alerts/<alertID>/acknowledge -H "Authorization: Bearer $TOKEN"
curl -s -X POST $BASE/api/v1/payment-alerts/<alertID>/resolve -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"note":"已核对渠道账单"}'

# 排期：资源 → 周规则（周一至周日 09:00–18:00）→ 可用性查询
curl -s -X POST $BASE/api/v1/resources -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"kind":"photographer","name":"主摄","buffer_before_minutes":30}'
curl -s -X PUT $BASE/api/v1/resources/<resourceID>/availability -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"expected_version":1,"rules":[{"weekday":1,"start_minute":540,"end_minute":1080},{"weekday":2,"start_minute":540,"end_minute":1080},{"weekday":3,"start_minute":540,"end_minute":1080},{"weekday":4,"start_minute":540,"end_minute":1080},{"weekday":5,"start_minute":540,"end_minute":1080},{"weekday":6,"start_minute":540,"end_minute":1080},{"weekday":7,"start_minute":540,"end_minute":1080}]}'
curl -s "$BASE/api/v1/schedule/availability?kind=photographer&from=2026-11-02T00:00:00Z&to=2026-11-09T00:00:00Z" \
  -H "Authorization: Bearer $TOKEN"

# 锁位（30m TTL）→ 定金回调自动确认；手动确认需定金已付
curl -s -X POST $BASE/api/v1/bookings -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"project_id":"<projectID>","resource_ids":["<resourceID>"],"starts_at":"2026-11-03T02:00:00Z","ends_at":"2026-11-03T04:00:00Z"}'
curl -s -X POST $BASE/api/v1/bookings/<bookingID>/confirm -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"expected_version":1}'
# 改期 / 取消（都需 expected_version；取消不退款）
curl -s -X POST $BASE/api/v1/bookings/<bookingID>/reschedule -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"expected_version":2,"starts_at":"2026-11-04T02:00:00Z","ends_at":"2026-11-04T04:00:00Z"}'
curl -s -X POST $BASE/api/v1/bookings/<bookingID>/cancel -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"expected_version":3,"reason":"客户改期"}'

# 拍前准备：shot list / 器材 / 授权（写窗口 held/confirmed；取消后只读）
curl -s -X POST $BASE/api/v1/bookings/<bookingID>/shot-list -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"title":"婚礼当天","note":"室内外交替"}'
curl -s -X POST $BASE/api/v1/shot-lists/<shotListID>/items -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"category":"ceremony","title":"交换戒指","reference_url":"https://example.com/ref.jpg","priority":"high"}'
curl -s -X PATCH $BASE/api/v1/shot-items/<shotItemID> -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"expected_version":1,"done":true}'
curl -s -X DELETE "$BASE/api/v1/shot-items/<shotItemID>?expected_version=2" -H "Authorization: Bearer $TOKEN"

curl -s -X POST $BASE/api/v1/bookings/<bookingID>/gear -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"resource_id":"<equipmentResourceID>","category":"camera","quantity":2}'
curl -s -X PATCH $BASE/api/v1/gear/<gearID> -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"expected_version":1,"checked":true}'

curl -s -X POST $BASE/api/v1/bookings/<bookingID>/authorizations -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"kind":"model_release","subject":"新娘 张女士"}'
curl -s -X PATCH $BASE/api/v1/authorizations/<authorizationID> -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"expected_version":1,"status":"signed"}'

# call sheet：仅 confirmed 可冻结；返回版本化快照与 SHA-256，HTML 为可打印预览
curl -s -X POST $BASE/api/v1/bookings/<bookingID>/call-sheets -H "Authorization: Bearer $TOKEN"
curl -s $BASE/api/v1/call-sheets/<callSheetID> -H "Authorization: Bearer $TOKEN"
curl -s $BASE/api/v1/call-sheets/<callSheetID>/html -H "Authorization: Bearer $TOKEN" > call-sheet.html

# 站内提醒：列表（type/status/booking_id/unread）与已读
curl -s "$BASE/api/v1/notifications?booking_id=<bookingID>&status=pending" -H "Authorization: Bearer $TOKEN"
curl -s -X POST $BASE/api/v1/notifications/<notificationID>/read -H "Authorization: Bearer $TOKEN"
```

## Windows Agent（M4）

### 构建入口

`agent/` 是独立 Go module（`photography-workflow/agent`，不依赖 `server`）。三个入口共享 `internal/` 核心：Wails GUI（`main.go`，`//go:build wails`）、无头 CLI（`cmd/pwf-agent`）、启动器（`cmd/pwf-launcher`），另含发布签名工具 `cmd/pwf-release`。

依赖：Go 1.26+、Node（前端）、Wails CLI v2、NSIS 3.x（仅打包）。

```bash
make agent-test       # CGO_ENABLED=0 go test ./...
make agent-cli        # agent/build/bin/pwf-agent
make agent-gui        # 前端构建 + wails build -tags wails -s -skipbindings
make agent-launcher   # launcher.exe + pwf-release.exe
make agent-release VERSION=0.5.0 CHANNEL=dev \
     MANIFEST_URL=https://download.example.com/agent/windows/dev/manifest.json \
     PUBLIC_KEY=<base64>
```

GUI 入口带 `wails` 构建标签，`frontend/dist` 是构建产物，**必须**用 `wails build -tags wails`（或 `make agent-gui`）；不带标签只会构建 CLI 桩，不会产出界面。前端类型检查：`cd agent/frontend && npm run build`（`vue-tsc` + Vite）。`wails dev` 生成绑定时会对 `model.Import` 报 reserved-keyword 警告；前端使用手写 DTO，所有构建与发布命令都传 `-skipbindings`，因此该警告只在开发期出现、不进入产物。

### 运行数据

`%LOCALAPPDATA%\PWF\agent\` 下保存 `state.db`（SQLite 状态机与任务）、`settings.json`、`logs\`（10MB×5 轮转）与 `cache\previews\`。`PWF_AGENT_DATA` 可整体重定位，`PWF_AGENT_DB` 单独覆盖数据库路径（CLI/CI 使用）。卸载安装包不会删除该目录。

### 安装包布局

NSIS 为每用户安装（无 UAC），默认安装到 `%LOCALAPPDATA%\Programs\PWF Agent\`：

```
launcher.exe            读取 current.txt 启动对应版本（快捷方式指向它）
current.txt             "<version>\r\n"
<version>\PWF Agent.exe GUI
<version>\pwf-agent.exe 无头 CLI
<version>\tools\        exiftool.exe / ffmpeg.exe（可选，随包附带）
licenses\               第三方许可证
uninstall.exe
.pwf-install            安装标记；只有存在该标记时卸载才删除整个目录
```

更新只写入新的版本目录并原子替换 `current.txt`，因此可在旧版本运行时安装；回滚同样只是切换 `current.txt`。安装包可选捆绑 WebView2 Evergreen bootstrapper，检测不到运行时会在交互安装中提示、静默安装中记录日志（假定系统已具备）。卸载保留 `%LOCALAPPDATA%\PWF\agent` 中的导入记录与设置。

### 更新信任链

- 清单 `manifest.json` 是 Ed25519 签名的信封（version/channel/url/sha256/notes/min_version/published_at），签名覆盖规范化 JSON；客户端内置公钥，下载前验签、下载后校验 SHA-256，并只接受 HTTPS 下载地址。
- 构建期通过 `-ldflags` 固化 `updater.BuildManifestURL/BuildPublicKey/BuildChannel/BuildSignerThumbprint`；`PWF_UPDATE_MANIFEST`、`PWF_UPDATE_PUBLIC_KEY`、`PWF_UPDATE_CHANNEL`、`PWF_UPDATE_SIGNER_THUMBPRINT` 可在测试或私有部署中覆盖。
- 检查时机：启动、每 6 小时、设置页手动检查；检查失败只显示错误，不影响导入与上传。
- **stable 失败关闭**：stable 通道除清单签名外还要求安装包具备 Authenticode 签名且签名者指纹与内置值一致（校验 WinVerifyTrust + 签名者证书，不信任目录签名）；没有代码签名证书时发布脚本直接失败，dev 包在 UI 中明确标记“未签名”，不允许自助安装。
- 回滚：`current.txt` 指向的版本连续两次启动未到达健康状态时，启动器切换回上一版本、写入回滚提示（启动后转为 warning 告警）并重启；`PruneVersions` 只保留当前与上一版本，其余版本目录在下次健康启动后清理。
- 发布流程：`pwf-release keygen -out release-key.json`（私钥只留在发布机，权限 0600）→ 构建安装包 → `signtool sign`（stable）→ `pwf-release sign -key ... -installer ... -version ... -channel ... -url ... [-signer-thumbprint ...] -out manifest.json` → 上传安装包、`manifest.json` 与 `SHA256SUMS.txt`。`pwf-release verify` 可在发布前用公钥复验清单，`check-signature` 可核对安装包签名者。

运维排查：`pwf-agent update-check -json` 退出码 `0` 已是最新、`2` 有更新、`1` 出错，便于脚本化巡检；`make agent-release` 产出在 `agent/dist/<version>/`。

### 验收与浸泡

自动门禁（Linux/Windows CI 与本地均可用）：

```bash
make agent-test   # 含 Windows 长路径导入测试（目标路径 > MAX_PATH，走 \\?\ 适配）
make agent-e2e    # CLI 双目的地夹具导入、逐文件 SHA-256 比对、重复插卡拒绝、status/list、selftest
pwf-agent selftest  # 迁移、双写三方哈希、拔卡暂停恢复、重启恢复
```

发布前的人工浸泡（读卡器/UNC/配额卷无法在 CI 上可靠复现）：

- **进程崩溃**：导入或上传进行中 `taskkill /F /IM pwf-agent.exe`，再 `pwf-agent resume -id <id>` / `pwf-agent upload -id <id>`；应清理 `.pwf-*.part`、复用分片会话、已校验文件不重拷、目标目录不出现重复 final 文件。GUI 侧同样可用任务管理器结束进程后重启验证。
- **拔卡**：拷贝中拔出读卡器，导入应暂停并写入 `card_removed` 告警且不产生 final 文件；重新插卡后 `resume` 继续，已校验文件跳过。
- **磁盘满**：把目的地指向配额卷或小容量 U 盘，写满后应立即暂停（`disk_full`，持久告警）且不做无限重试；释放空间后 `resume`。
- **UNC/网络盘**：`-a \\nas\share\photos -b \\nas2\share\photos` 运行导入与上传，中途断开共享验证 `offline` 暂停与恢复；同时观察长路径（>260 字符）项目结构可读写。
- **重复插卡**：同一张卡连续导入两次，第二次应以退出码 4 拒绝；再次导入到服务端时资产数不变（服务端按内容哈希去重，smoke 已断言）。

## 图库与选片（M5）

链路：摄影师在 Agent 上传原图 → Agent 生成并上传 **gallery JPEG**（长边 ≤2560）→ 服务端派生 thumb/display/LQIP → 管理端建相册、选片、发布 → 生成一次性分享 token → 客户在手机 H5 浏览、收藏/淘汰、提交 → 管理端查看结果。图片字节从不经过 API，只下发短期签名 URL。

### 数据模型与权限（迁移 `0015_galleries.sql` + `0016_gallery_watermark.sql`）

- `galleries` 增列 `version`（乐观锁）、`selection_version`（客户选片版本）、`submitted_at`、`revoked_at`、`manifest_key`、`watermark_text`（发布前快照，空则用租户名）、`updated_at`；`expires_at` 同时是访问与选片截止。状态仍是 `draft/published/expired/revoked`。
- `gallery_assets`（`(gallery_id, asset_id)` 主键 + `(gallery_id, position)` 唯一）、`gallery_tokens`（**只存 SHA-256**，`max_visits/visits/expires_at/revoked_at`）、`gallery_selections`（`selected/rejected`，`UNIQUE(gallery_id, asset_id)`）。
- `asset_derivatives` 增列 `meta jsonb`（LQIP data URI、主色、源尺寸）。
- 新表全部 `ENABLE + FORCE ROW LEVEL SECURITY`；`gallery_tokens` 例外：只 ENABLE 不 FORCE，因为客户会话交换发生在租户确定之前 —— 由 `SECURITY DEFINER` 函数 `gallery_token_lookup(hash)`（固定 `search_path`，仅 `pwf_app` 可执行）完成哈希反查，应用直接查表仍受 RLS 限制。
- Worker 新增 `galleries`/`gallery_assets`/`gallery_tokens`/`asset_derivatives` 读写与 `notifications` INSERT 授权，均配 `TO pwf_worker USING (true)` 宽松策略；其余业务表权限不变。
- `notifications.type` 增加 `gallery_deadline`、`gallery_submitted`。

### 派生图管线（`internal/media`）

- 类型与规格：`gallery`（Agent 上传的源 JPEG，长边 ≤2560、q82）、`thumb`（长边 640、q78、**无水印**）、`display`（长边 2048、q80、**右下角文字水印**）。LQIP 为 24px JPEG q45 的 base64 data URI，另存主色。
- 水印：`golang.org/x/image/font/opentype` 加载 TTF/OTF/TTC；字号约为短边 3.5%，白色 55% + 深色描边。字体取 `PWF_GALLERY__WATERMARK_FONT`，未配置时使用内嵌 Latin 字体；生产环境该配置必填（`CheckProduction`）。
- 水印文字在**发布前**从相册快照写入（缺省取 `tenants.name`）；发布后 PATCH 修改水印返回 409 `invalid_state`（display 图已固化），要改水印必须新建轮次。
- `media.derive` 任务：读 gallery 源 → 生成三件套 → `Put` 对象 → 事务写 `asset_derivatives`（`ON CONFLICT DO UPDATE`）→ 资产 `verified → derivative_ready` → Outbox `asset.derivative_ready`。已有完整派生直接跳过；失败写 `meta.error` 并告警，资产保持 `verified`，发布时计入缺失。
- 补齐入口 `POST /api/v1/projects/{id}/derivatives:derive`（`force` 全量重建），覆盖旧资产与 JPEG/PNG/WebP 原图；RAW 必须由新版 Agent 上传 gallery 预览。
- 视频：display 使用封面帧，manifest 标记 `is_video`，灯箱提示交付阶段提供。

### Agent 派生图上传

- `internal/preview` 新增 `Gallery(...)`：RAW 走 exiftool 内嵌预览，照片走 imaging/ffmpeg，缓存到 `%LOCALAPPDATA%\PWF\agent\cache\previews\<sha>\gallery.jpg`。
- 原图 `remote_verified` 后，SQLite 新增 `derivatives` 表（`pending/uploading/uploaded/failed`）驱动上传：`POST /agent/v1/derivatives:presign`（服务端按内容哈希生成 key，天然幂等）→ 单次 PUT（≤16MiB）→ `POST /agent/v1/derivatives/{assetID}:complete`（服务端 `Stat` + 流式 SHA-256 复核，写 `kind=gallery` 派生并入队 `media.derive`）。
- 派生失败只记 `derivative_upload_failed`，**不阻塞原图上传与批次完成**；断点续传复用 `derivatives` 行；CLI `upload` 摘要输出 `derivatives=<ready>/<failed>`，GUI 文件行显示预览状态。
- 旧版 Agent 不传预览：该批 RAW 会在发布时以 `derivatives_incomplete` 被拒；升级 Agent 后重新导入或走批量补齐。

### 发布与不可变 manifest

- `POST /api/v1/galleries/{id}/publish` 是同步接口：先校验全部资产 `thumb`+`display` 就绪，缺失返回 409 `derivatives_incomplete`（带缺失数量与样例资产 ID）。
- 通过后组装不可变 manifest（版本、条目 key/尺寸/LQIP 哈希；**不含 URL**），写入 `t/<tenant>/galleries/<id>/v<N>/manifest.json`，事务内置 `published`、`manifest_key`、审计与 Outbox `gallery.published`；同 key 重发幂等覆盖。
- 发布即冻结资产集合；改选需新建相册（`round_no` 按项目自动 +1）。
- 必须追加派生（例如补传预览后重建）：先 `derivatives:derive` 再重新发布。

### 分享链接、会话与客户 API（`/g/v1`）

- `POST /api/v1/galleries/{id}/tokens` 返回**仅此一次的明文 token**（32B base64url，库存 SHA-256）与 `gallery_url`。链接形如 `https://<public_base_url>/#/a/<token>`：token 只在 URL fragment，浏览器不会发给服务器，也不进入 API 日志与 `Referer`。
- `POST /g/v1/session` `{token}`：哈希反查 → 校验 published、未过期/撤销、访问次数未超限 → 自增访问并返回 2h 会话 JWT（封顶 token 过期）+ 相册元信息；错误码 `invalid_token/expired/revoked/too_many_visits`。
- 会话每个请求都回查 token 与相册状态，撤销 token/相册**立即生效**；响应统一 `Cache-Control: no-store` + `Referrer-Policy: no-referrer`，客户路由按会话 + IP 固定窗口限流 120 req/min，不记录请求体。
- `GET /g/v1/albums/{albumID}/manifest?cursor=&limit=≤60`：按 `position` keyset 分页，返回 10 分钟有效的 `thumb_url/display_url`、LQIP、主色、宽高、`is_video`、当前 decision 与 `selection_version`。
- `POST /g/v1/selections` `{expected_version, items:[{asset_id, decision:selected|rejected|none}]}`（≤200，支持 `Idempotency-Key`）：版本不符 409 `version_conflict`（Problem 带 `current_version`）、过期 410 `gallery_expired`、超上限 409 `selection_limit_exceeded`、已提交 409 `already_submitted`；成功后 `selection_version+1`、审计（系统 actor + IP）与 Outbox。
- `POST /g/v1/albums/{albumID}/submit` `{expected_version}`：校验截止与上限 → 写 `submitted_at`、资产状态、`gallery_submitted` 站内通知、审计与 Outbox；重复提交 200 幂等。
- 幂等中间件的租户来源抽象为 `auth.TenantFrom(ctx)`，用户与相册会话共用同一套 `Idempotency-Key` 语义。

### 管理 API（owner/staff/assistant）

- `GET /api/v1/galleries?project_id=&status=&limit=&offset=`、`POST /api/v1/galleries`（`round_no` 自动递增）、`GET/PATCH /api/v1/galleries/{id}`（PATCH 走 `expected_version`，发布后可延期并审计）
- `GET/POST /api/v1/galleries/{id}/assets`、`DELETE /api/v1/galleries/{id}/assets/{assetID}`（仅 draft；同项目且 `verified/derivative_ready`）、`GET /api/v1/galleries/{id}/candidates`（项目内候选资产）
- `POST /api/v1/galleries/{id}/publish`、`POST /api/v1/galleries/{id}/tokens`、`DELETE /api/v1/galleries/{id}/tokens/{tokenID}`
- `POST /api/v1/galleries/{id}/revoke`（撤销全部 token）、`POST /api/v1/galleries/{id}/reopen`（清 `submitted_at`，允许客户继续修改）
- `GET /api/v1/projects/{id}/assets` 扩展返回 10 分钟预签名 `thumb_url`（派生缺失时为空），供选择器使用。

### 截止与通知（Worker）

- `gallery-sweep` 每 60 秒：`published` 且过期 → `expired`、撤销全部 token、审计与 Outbox（系统 actor）；24 小时内到期且未提醒过 → 插入 `gallery_deadline` 站内通知（`gallery_deadline:<id>` 去重），由现有通知投递循环发送/重试，Webhook 复用 `notify.webhook`。
- `gallery_submitted` 由客户提交事务内写入（`subject_type=gallery`）；通知投递前复核相册仍 open，已撤销/过期的一律 `skipped`。

### 两个前端

- `web-gallery`（客户 H5，移动优先）：hash 路由 `/#/a/<token>`；标题/倒计时/已选计数与上限；全部/已选/已弃筛选；行级虚拟滚动（可见行 ±2）+ `IntersectionObserver` 懒加载 + LQIP 模糊占位 + 相邻预取；灯箱显示带水印 display 图，左右滑动并预取相邻，收藏/淘汰乐观更新，409 时拉最新版本重放一次，仍冲突提示刷新；批量提交确认；已提交/已过期/已撤销/链接无效四种终态页；不提供下载入口。
- `web-admin`（最小管理端）：登录（access token 只在内存，refresh 持久化并轮换，401 自动刷新）、相册列表/创建、详情页选择资产（缩略图、全选、搜索）、派生缺失提示与一键补齐、发布、token 创建/复制/撤销与访问统计、结果查看（已选/已弃/未决定、提交时间）、延期、撤销、重新开放。不含项目/合同/支付模块，那些继续走 API。

### 运行、验收与运维

```bash
make web-test           # 管理端/客户 H5 单测与类型检查，及小程序类型检查
make web-build          # 管理端、客户 H5 和微信小程序生产构建
make e2e-gallery        # 独立 compose 栈 + 真机浏览器全流程（选片 + 审片 + 交付，含 mailsink；约 6 分钟，首次需下载 Chromium）
make migration-matrix   # 0019→…→0015 down、重新 up、0019/0018/0017/0015 带数据时拒绝回滚
make smoke              # M0–M7 服务端全生命周期（含发布闸门/token/选片/撤销/审片/批注/修订/交付/归档/邮件）
```

`scripts/gallery-e2e.sh` 使用独立项目与端口（Postgres `55434`、MinIO `59006/59007`、API `18084`、admin `4175`、gallery `4174`），流程：seed 登录 → 建项目并上传 4 张 JPEG → 建相册、加 2 张 → 发布断言 409 → 一键补齐 → 发布 → 发 token → 移动端打开分享链接 → 收藏/淘汰、灯箱校验水印图 → 提交 → 管理端看到结果；随后依次执行审片链路（批注/提修改/接单/上传/回传/通过）与交付链路（API 播种付费 + approved 轮次 → 管理端建交付/构建/签发 → 移动端下载 web 包与原图并校验 SHA → 撤销后终态页 → 邮件含 `/#/d/`）。失败日志落在 `deploy/.ci-gallery/`；`PWF_E2E_SKIP_INSTALL=1` 可跳过 `npm ci`。CI 由 `PWF_CI_WEB=1` 在 integration job 中启用，`web` job 跑 `web-admin`（官网、认证与管理端）和 `web-gallery` 的类型检查、单测与构建，不再单独构建 `web-site`。

部署要求：

- `PWF_GALLERY__PUBLIC_BASE_URL`：客户 H5 的 https 源（生产必填），分享链接由它拼接。
- `PWF_GALLERY__WATERMARK_FONT`：覆盖水印文字的字体文件（生产必填；中文需部署 Noto Sans CJK 等）。字体缺失时服务启动失败；内嵌回退只有 Latin，CJK 会显示为占位方框，因此不能省。
- 生产图片分发应接 CDN 签名（CloudFront/阿里云等），当前使用 MinIO 预签名，已列为上线门禁。
- 已导入的旧 RAW 资产需要升级 Agent 后补传 gallery 预览，或对服务端可解码格式执行 `derivatives:derive`。

验收映射：发布前缺派生 → 409 且列出缺失；2,000 张相册 manifest 游标分页、虚拟滚动只渲染可视行、图片全部走签名 URL；选片批量幂等、版本冲突 409、超限 409、截止 410、重复提交不产生重复结果；撤销 token/相册后旧会话立即失效；水印只在 display、thumb 无水印、LQIP 先于大图；截止提醒进入通知队列、过期清扫落库审计；Agent 预览上传可续传，缺预览的 RAW 不会静默进相册。

## 后期集成与审片（M6）

链路：摄影师在已提交的选片上发起**审片轮次** → 系统签发独立审片链接并邮件客户 → 客户在手机上批注（点/矩形/椭圆/手绘）、评论、提修改或直接通过 → 员工接单生成修图任务 → 修图师通过 FolderWatch 目录回传或管理端手工上传**修图版本** → 员工重新提交 → 客户复核通过。每轮只允许一次修图往返；超出合同包含轮次必须挂已生效的 `extra_retouch` 变更单（owner 可带原因强制放行并审计）。

### 数据模型与权限（迁移 `0017_proofing.sql` + `0018_integrations_notify.sql`）

- `contracts.included_rounds`（默认 2，合同创建时冻结）；`clients.email` 用于审片邮件。
- `proof_rounds`（`round_no` 项目内唯一、`version` 乐观锁、`due_at/viewed_at/submitted_at/approved_at`、`change_order_id` 部分唯一索引保证一单只用一次、`override_by/override_reason`）与 `proof_round_items`（轮次快照）。DB 触发器 `proof_round_gate()` 对非空 `change_order_id` 再做一次“同项目 + applied + 含 extra_retouch 项”兜底。
- `proof_round_tokens` 结构同 `gallery_tokens`（库存 SHA-256；只 ENABLE 不 FORCE，经 `SECURITY DEFINER` 的 `proof_token_lookup(hash)` 反查）；`comments`（归一化 `annotation jsonb`、`client_ref` 幂等、`parent_id` 回复、`resolved_at/by`）；`retouch_tasks`/`retouch_task_items`（每轮最多一个活动任务）；`asset_revisions`（`UNIQUE(asset_id, revision_no)`、`UNIQUE(tenant_id, asset_id, sha256)`）与 `asset_revision_derivatives`；`integration_jobs`（`UNIQUE(tenant_id, retouch_task_id, direction)`）；`email_messages`（收件人入队时解析，worker 不读 `clients`）。
- 新表全部 `ENABLE + FORCE ROW LEVEL SECURITY`（`proof_round_tokens` 例外）；worker 获得审片/修订/集成/邮件相关表的宽松策略，仍禁止使用迁移账号。
- `notifications.type` 扩展 `proof_ready`、`proof_changes_requested`、`proof_resubmitted`、`proof_approved`、`proof_deadline`。

### 审片链接与会话安全

- 链接形如 `https://<web-gallery 源>/#/p/<token>`：token 只在 fragment，浏览器不会发给服务器；`POST /g/v1/proof/session` 换 4h 会话（封顶 token 过期），签发独立 issuer `pwf-proof`（claims `tid/rid/tk`），与员工 JWT、选片会话互不通用。
- token 默认有效到 `due_at + PWF_PROOF__TOKEN_GRACE`（168h），可单独撤销；每次请求回查 token、轮次与相册，撤销 token 后旧会话 401 `session_revoked`，撤销相册立即 410 `gallery_revoked`。**审片只要求相册未撤销，忽略选片截止 `expires_at`**。
- 客户路由统一 `Cache-Control: no-store`、`Referrer-Policy: no-referrer`、`nosniff`，会话 + IP 限流 120 req/min（换会话 30 req/min），不记录请求体；图片仍只下发 10–15 分钟预签名 URL。

### 轮次状态机与收费闸门

`open → in_review → changes_requested → retouching → resubmitted → approved`

- 客户首次拉 manifest：`open → in_review`（`viewed_at`）。`in_review` 下可直接通过或提修改；`retouching` 期间客户不能通过。
- `changes_requested → retouching` 由员工接单；`retouching → resubmitted` 要求**每个条目都有晚于接单时间且 `derivative_ready` 的 revision**，否则 409 `revisions_incomplete`（带缺失数与样例）。
- `resubmitted` 只能通过；再提修改 409 `round_cycle_exhausted`，必须新建轮次（可能触发额外收费）。已通过后任何提交 409 `invalid_state`；同决策同版本重放 200 幂等。
- 第 N 轮 > 合同 `included_rounds` 时：必须挂同项目 `applied` 且含 `extra_retouch` 项的变更单（一单只用一次）；否则 409 `extra_round_requires_change_order`。owner 可提交 ≥5 字 `override_reason` 强制放行，写 `override_by/reason` 与审计 `proof_round.gate_overridden`；其他角色 403 `owner_required`。
- 员工可 `PATCH /api/v1/proof-rounds/{id}` 延期（`expected_version`，仅未通过，且不得早于当前时间）。

### 批注与评论

- 归一化坐标 `annotation.v1`（0–1），point 恰 1 点、rect/ellipse 恰 2 点且 `x1<x2,y1<y2`、freehand 2–256 点，颜色 `#RRGGBB[AA]`，JSON ≤16 KiB；非法 422 `invalid_annotation`。坐标与分辨率/版本解耦，原图与修图版本共用。
- 客户可发纯文字、批注或回复（`parent_id` 限同轮次同资产），单条 ≤2000 字；`Idempotency-Key` 落 `client_ref`，重复重放返回原评论。客户只能解决**自己发起**的评论（员工评论 403 `comment_forbidden`），员工可解决任意评论；解决/创建均写审计与 outbox。
- 通过后评论只读；不提供编辑/删除已提交评论。

### 修图版本与派生

- 导入（`POST /api/v1/proof-rounds/{id}/revisions`，multipart）：声明 `asset_id/sha256/size/width/height/note` + 文件（JPEG/PNG/WebP，≤50 MiB）。服务端边写对象边算 SHA-256；声明不一致删对象并 422 `checksum_mismatch`；同资产同校验和 200 `duplicate:true`；否则 `revision_no = max+1`、源对象写 `t/<tenant>/revisions/<asset>/<rev>/source.<ext>`，入队 `media.derive_revision`。
- Worker 派生 thumb 640（无水印）+ display 2048（水印文字取相册快照，回退租户名）+ LQIP，写 `asset_revision_derivatives` 并置 `derivative_ready`，outbox `asset.revision_ready`；失败置 `failed` 并告警 `revision_derive_failed`。
- 审片 manifest 取**最新 `derivative_ready` revision** 的预览，无 revision 回退 M5 `asset_derivatives`；条目带 `revision_id/revision_no`，客户页显示「修图版 vN」。

### 外部修图对接（`internal/integration`）

- `Provider` 接口 `Name/Export/Poll`；工厂按 `PWF_INTEGRATIONS__PROVIDER` 构造。`manual`（管理端逐张上传，全部导入后任务自动 `delivered`）与 `folder_watch` 为完整实现；`ftp`/`pixcake`/`xiangxiang` 只保留接口占位，**选中即启动失败**（含生产），不会“配置了但静默不工作”。
- FolderWatch 目录约定（根目录 `PWF_INTEGRATIONS__FOLDER_WATCH__ROOT`）：
  - 导出：`outgoing/<taskID>/manifest.json` + `outgoing/<taskID>/source/<assetID>.<ext>`；
  - 回传：修图方把成品放到 `incoming/<taskID>/<assetID>.jpg|.png|.webp`；worker 每 30s 扫描，按 SHA-256 幂等导入并移到 `done/`，坏文件移到 `failed/` 并告警 `integration_import_failed`；
  - 超过 `overdue_after`（72h）未交付告警 `retouch_overdue`。
- 运维视图 `GET /api/v1/integrations`（provider、root、导出/导入计数、活动/超期任务、最近错误）；owner 可 `POST /api/v1/integrations/folder-watch/sync` 手动扫描一次。

### 通知（Worker）

- `email_messages` 队列 + `email.send` job（最多 5 次退避）：SMTP 支持 `starttls`/隐式 `tls`/`none`（仅本地 sink），发送成功**清空正文**（正文含一次性链接），最终失败 `dead` 并告警 `email_delivery_failed`；发送前重新确认轮次/相册可访问，已通过或已撤销则 `skipped`，绝不发送失效链接。
- 模板（代码内中文）：审片邀约、修图已回传、截止提醒。链接由 `PWF_GALLERY__PUBLIC_BASE_URL + /#/p/<token>` 拼接；日志不打印链接与正文。
- `proof-sweep` 每 60 秒：`due_at` 24h 内且未通过/未撤销、尚无 `proof_deadline:<id>` 通知 → 员工站内通知 + 客户邮件。
- 站内/Webhook：`proof_changes_requested`、`proof_approved` 写 `notifications`（`subject_type=proof_round`）+ outbox `proof_round.*`，沿用 `notice.deliver` 的重试与签名。
- 短信：`notify.sms.Provider` 接口 + `sandbox`（只记录不真发）；生产拒绝 sandbox 与未知 provider。管理端 `/notify` 指定 sms 且未配置时 409 `channel_unavailable`。
- 重发：`POST /api/v1/proof-rounds/{id}/notify` 每次新建 token（旧明文不可恢复），返回 202 + 新链接。

### 管理端与客户页

- `web-admin`：路由 `/proofs`、`/p/:id`；列表按项目/状态过滤；详情含状态时间线、延期、token 签发/撤销/复制、发送邮件、资产网格（修图版标识、批注计数）、带批注叠加的只读灯箱与评论线程、接单、多文件上传（已导入/缺失）、重新提交；建轮次对话框显示合同包含轮次，超出时要求选择 `extra_retouch` 变更单或 owner 填写强制放行原因。
- `web-gallery`：路由 `/#/p/<token>`；移动优先的网格（LQIP + 懒加载）、灯箱批注工具（点/矩形/椭圆/自由手绘，归一化坐标）、评论/回复/解决（乐观更新，`version_conflict` 自动重放一次）、底部「通过 / 提出修改」、提交后只读与通过/关闭/链接无效/已撤销终态页；「修图版 vN」徽标与“每轮仅一次修改往返”提示。

### 运行、验收与运维

```bash
make e2e-gallery        # 独立栈 + 选片/审片/交付 E2E（含 mailsink 邮件断言）
make migration-matrix   # 0019→…→0015 down、重新 up、0019/0018/0017/0015 带数据时拒绝回滚
make smoke              # M0–M7 服务端全生命周期（含闸门/批注/修订/幂等/撤销/交付/归档/邮件队列）
make web-test           # 两个前端 typecheck + vitest
```

`make ci`（`scripts/ci.sh`）现在额外启动 mailsink（默认 `18082`），以 staff 账号验证非 owner override 403，并在 smoke 结束后直接用 superuser DSN 断言：`email_messages` 已发送行正文为空、`audit_logs` 存在 `proof_round.gate_overridden`。`scripts/gallery-e2e.sh` 增加 mailsink（`19025`）与 `web-gallery/e2e/proof.e2e.mjs`：管理端建轮次/发链接 → 客户批注/提修改 → 管理端接单/上传修图/回传 → 客户通过，并断言无控制台/网络错误、邮件正文包含 `/#/p/`。失败日志落在 `deploy/.ci/` 与 `deploy/.ci-gallery/`。

部署要求：

- `PWF_GALLERY__PUBLIC_BASE_URL` 同时用于选片与审片链接（生产必须 https）。
- `PWF_NOTIFY__EMAIL__ENABLED=true` 且配置 host/from；生产强制 STARTTLS 或隐式 TLS。发件域的 SPF/DKIM 属上线门禁。
- FolderWatch 的根目录必须是稳定的本机/网络共享路径，并同时挂载给修图方；`incoming/` 只接受 JPEG/PNG/WebP。
- 真实短信、FTP、PixCake、享像派尚未实现：分别需要适配器，配置占位 provider 会启动失败。

验收映射：批注四类形状归一化落库、评论/回复/解决审计；修订带校验和与递增版本号、重复导入幂等、篡改 422；未付费/未签变更单不能开始额外轮次、owner override 留痕；FolderWatch 导出/回传自动生成修订、手工上传可用、失败/超时告警；邮件入队可重试、发送后正文清空；审片链接独立于选片截止、撤销立即失效；管理端与客户页可完成建轮次→接单→回传→通过。

## 交付下载与归档（M7）

链路：审片通过 → 员工创建交付 → Worker 渲染 web 版并打包 `web`/`original` 两个 ZIP → 款清后签发下载链接并邮件客户 → 客户在 H5 逐包/逐张下载 → 项目归档后 Worker 逐对象校验并计算保留期 → 到期前告警、owner 审批删除。

### 数据模型与权限（迁移 `0019_delivery_archive.sql`）

- `deliveries`（每个 `approved` 审片轮次最多一条，`build_version` 版本化）、`delivery_items`（最终文件来源快照：最新 `derivative_ready` 修订源，否则原图；含 web 派生 key/尺寸/校验和）、`delivery_packages`（`web`/`original` 每版本各一行，`manifest jsonb`）、`download_tokens`（库存 SHA-256，`delivery_token_lookup(hash)` `SECURITY DEFINER` 反查租户）、`archives`/`archive_items`（逐对象预期与实测 SHA-256/大小）、`retention_policies`（租户级，迁移为现有租户回填默认行）。
- 业务表 `ENABLE + FORCE ROW LEVEL SECURITY`；`download_tokens` 只 ENABLE（同 `gallery_tokens`/`proof_round_tokens`）。Worker 授权按表最小化：`delivery_items`/`delivery_packages` 含 `DELETE`（重建先重置条目与包），`archives`/`archive_items` 不含 `DELETE`（行是墓碑，删除对象不删行），`projects` 只有 `SELECT` + **列级** `UPDATE (id)`——finalize 需要用 `FOR SHARE` 锁项目行（PostgreSQL 要求 UPDATE 权限），但不能写任何项目数据。
- `notifications.type` 扩展 `delivery_ready`、`archive_ready`、`archive_retention_warning`、`archive_deleted`；Down 迁移在交付/归档表有行时拒绝回滚。

### 构建流水线

- `POST /api/v1/deliveries` `{proof_round_id}`：轮次必须 `approved`、项目非 `archived/cancelled`；同轮重复创建返回既有行。
- `POST /api/v1/deliveries/{id}/build`：要求 `projects.payment_status='paid'`；`build_version + 1`、`status='building'`、入队 `delivery.build`（202）。构建中再次请求 409 `build_in_progress`；未付款 409 `payment_required`。
- job 串：`delivery.build` 快照本轮条目后逐项入队 `delivery.render_web`（长边 2048、透明铺白、无水印 JPEG q82；HEIC/RAW/TIFF 跳过并写 `web_spec_unsupported_mime`，解码失败写 `web_spec_decode_failed`），全部终态后每个 spec 一个 `delivery.package`。所有 job 带 `build_version`，版本不匹配即 no-op，天然幂等。
- ZIP：entry 用 `Store`（原图字节不变）、时间固定 1980-01-01（可复现）、entry 名清洗为 `<project_code>_<position>_<stem><ext>`；根目录与 `delivery_packages.manifest` 双写 `manifest.json`（每项 SHA-256/大小/来源 revision|original/尺寸/warning）。原图复制时边算哈希，与快照不符立即失败 `original_checksum_mismatch`。临时文件 `Sync → Stat → Seek → Put → 删除`，Worker 每小时清理 24h 前残留。
- finalize 在 `FOR UPDATE OF deliveries … FOR SHARE OF projects, proof_rounds` 下重查支付/项目/轮次：退款已发生 → `blocked`（`payment_required`）不对外暴露；两个包都 ready 才置 `ready`、条目资产置 `delivered`、写站内通知 `delivery_ready` 与 outbox `delivery.ready`。永久失败置 `failed` + `delivery_build_failed` 告警；任一包任务重试都会再次收敛 finalize，不会卡在 `building`。

### 下载授权与尾款闸门

- 链接 `/#/d/<token>`，token 只在 fragment；`POST /g/v1/delivery/session` 换 30m 会话（封顶 token 过期），独立 issuer `pwf-delivery`（claims `tid/did/tk`），与员工 JWT、选片会话、审片会话互不通用；URL 中的 delivery 与会话 claim 不一致 403 `delivery_mismatch`。
- 每次请求（会话/摘要/列表/下载）按固定顺序校验：token 未撤销/未过期/未超次数 → 项目状态 → `paid` → 轮次 `approved` → delivery `ready`。项目 `archived` 410 `delivery_archived`、`cancelled` 410 `delivery_cancelled`，退款后下一次请求即 409 `payment_required`；**owner 也没有 override**。
- 下载 `POST /g/v1/delivery/downloads`（`{package_id}` 或 `{item_id,spec}`）：在 token 行 `FOR UPDATE` 下原子递增次数，超 `max_downloads` 409 `downloads_exhausted`；返回 5 分钟预签名 URL。`storage.NamedPresigner` 可选接口给 MinIO 追加 `response-content-disposition`（附件文件名），未实现时回退普通 `PresignGet`；字节永不经过 API。
- 签发 `POST /api/v1/deliveries/{id}/tokens`：有效期 1–30 天（受 `max_token_ttl` 限制，默认 14 天）、次数 1–100（默认 20）；明文只在创建响应出现一次。`revoke` / `revoke-all` 立即生效（每次请求回查）。`POST /{id}/notify` 每次新建 token 并发邮件，正文发送成功后清空；发送前复核 delivery/token/payment/round，失效则 `skipped`。

### 归档与保留

- `POST /api/v1/archives` `{project_id}`：项目必须 `state='archived'`，幂等；Worker 每 5 分钟为已归档但缺归档行的项目补建。
- `archive.verify` 在事务内快照九类对象（原图、M5 派生、修图源与派生、交付 web 文件与 ZIP、合同文档/签署件、发票文件），逐项 `archive.verify_item` 流式 SHA-256；缺失 `missing`、哈希/大小不符 `mismatch`。全部终态后汇总：`ready` 计算 `retention_until = now + keep_days`、资产置 `archived`、通知与 outbox；`failed` 保存样例并告警 `archive_verify_failed`。核验重试复用快照，已 verified 的不重算。
- 保留：`GET/PUT /api/v1/retention-policy`（owner，`keep_days` 1–3650、`warn_days` < `keep_days`，默认 365/30）；Worker 每日扫描 `retention_until - warn_days <= now` 且未告警的 ready 归档，发一次 `archive_retention_warning`，**从不自动删除**；归档创建时快照策略，之后改策略不影响既有保留期。
- 删除：`POST /api/v1/archives/{id}/deletion`（仅 owner，`{confirm:"DELETE",reason}`，归档必须 `ready` 且项目 `archived`）→ `deleting`，逐对象 `archive.delete_item`（幂等，跳过仍被其它归档引用的 key 并告警）→ `deleted` + `deleted_at`，行作为墓碑保留；数据库行（合同/支付/发票/审计/交付记录）永不删除。

### 管理端与客户页

- `web-admin`：`/deliveries`（按项目/状态过滤、付款闸门、构建进度、下载次数）、`/deliveries/:id`（包大小/SHA/entry 数、warning 列表、构建/重建、token 签发一次性明文/撤销/全撤销、通知客户）、`/archives/:id`（核验汇总、失败项与对象 key、保留倒计时、owner 删除对话框要求输入 `DELETE` + 原因）、`/settings/retention`（owner）；审片通过后详情页出现「交付下载」入口。
- `web-gallery`：`/#/d/<token>`，移动优先的包卡片（网页版/原图）+ 逐张下载、剩余次数显示、终态页（未结清/过期/撤销/次数耗尽/已归档/未就绪/未生成）；会话交换后 `history.replaceState` 去掉 fragment，请求 `credentials:'omit'`、`referrerPolicy:'no-referrer'`。

### 运行、验收与运维

```bash
make smoke            # 追加 M7：未付款建交付/构建/签发 409 → 付款后构建两个包并读 ZIP manifest → 会话/包/逐张下载 → 撤销 410 → 退款后 409 → 归档核验/保留期/仅 owner 删除闸门
make e2e-gallery      # 追加交付 E2E：管理端建交付/构建/签发 → 客户下载 web 包与原图（校验 SHA）→ 撤销后终态页 → 邮件含 /#/d/
make migration-matrix # 0019 up/down/up、保留策略回填、通知 CHECK、带数据拒绝回滚、worker 权限（列级 UPDATE + FOR SHARE 可执行、archives 无 DELETE）
make loadtest         # 同 scripts/loadtest.sh，可追加 -scenario/-concurrency/-duration/-json-out
make payment-replay   # 同 scripts/payment-replay.sh，可追加 -project/-payment-id/-delivery-token
make launch-drill     # 一条命令起隔离栈：造待支付订单 → 支付/退款各重放 200 次 → 普通 API 压测 + 200 并发抢档；报告落在 deploy/.drill
bash scripts/loadtest.sh        # 压测：选片写 / 普通 API / 200 并发抢档（仅 1 成功）
bash scripts/payment-replay.sh  # 同一 payment.succeeded 顺序/并发各 100 次仅入账一次；退款重放仅生效一次
```

上线验证工具只允许对**测试渠道/测试数据**执行，禁止指向生产真钱渠道。故障处置见 `docs/runbook-delivery-archive.md`（构建失败、token 泄露、核验失败、删除审批、回滚），灾备见 `docs/dr.md`（RTO/RPO、PITR、对象版本化、恢复顺序与校验、演练清单）。

部署要求：

- `PWF_DELIVERY__PUBLIC_BASE_URL`（留空回退 `PWF_GALLERY__PUBLIC_BASE_URL`；生产设置时必须 https）。
- Worker 的 `PWF_DELIVERY__TEMP_DIR` 需可写且容量足够（ZIP 按原图字节拼接，`max_assets × 单文件大小`）。
- 归档核验需要 Worker 能读对象存储；对象删除不可逆，生产必须开启 bucket 版本化（或对象锁）作为兜底。

## 配置

`server/config/config.yaml` 是本地默认值。生产一律用环境变量覆盖，规则：`PWF_` 前缀、`__` 表示层级。

| 环境变量 | 说明 |
|---|---|
| `PWF_ENV` | 运行环境：`development` / `production` / `test`（默认 `development`）。`production` 下启动时拒绝 `sandbox` 支付/电子签适配器（含留空默认）与示例密钥 |
| `PWF_DATABASE__URL` | API 连接串（`pwf_app`，RLS 生效） |
| `PWF_DATABASE__MIGRATE_URL` | 迁移/seed 连接串（owner），留空时回落到 `URL` |
| `PWF_DATABASE__WORKER_URL` | Worker 专用连接串（生产必填，禁止使用迁移 owner） |
| `PWF_AUTH__JWT_SECRET` | JWT 签名密钥（生产必须替换为随机值，至少 32 字符；示例值会被拒绝） |
| `PWF_HTTP__REQUEST_TIMEOUT` | 单请求处理上限（默认 `15s`，超时返回 504 `timeout`） |
| `PWF_WORKER__POLL_INTERVAL` | Worker 轮询间隔（默认 `2s`） |
| `PWF_WORKER__BATCH_SIZE` | 每轮领取的任务数（默认 `10`） |
| `PWF_STORAGE__ENDPOINT` / `__ACCESS_KEY` / `__SECRET_KEY` / `__BUCKET` | 对象存储（生产拒绝 `pwf-secret` 等示例密钥） |
| `PWF_STORAGE__PUBLIC_BASE_URL` | 生成资产公开 URL 的前缀 |
| `PWF_ESIGN__PROVIDER` | 电子签适配器（当前仅 `sandbox`；生产厂商适配器待凭据） |
| `PWF_ESIGN__CALLBACK_SECRET` | 回调 HMAC 密钥（生产必须替换；`make smoke` 的 `-esign-secret` 需与之一致） |
| `PWF_ESIGN__SIGN_BASE_URL` | sandbox 签署页地址前缀 |
| `PWF_ESIGN__CALLBACK_TOLERANCE` | 回调时间窗（默认 `5m`） |
| `PWF_PAYMENT__PROVIDER` | 支付适配器（当前仅 `sandbox`；微信/支付宝待凭据） |
| `PWF_PAYMENT__CALLBACK_SECRET` | 支付回调 HMAC 密钥（生产必须替换；`make smoke` 的 `-payment-secret` 需与之一致） |
| `PWF_PAYMENT__MERCHANT_ID` | 商户号，回调必须匹配（`-payment-merchant`） |
| `PWF_PAYMENT__PAY_BASE_URL` | sandbox 支付页地址前缀 |
| `PWF_PAYMENT__CALLBACK_TOLERANCE` | 支付回调时间窗（默认 `5m`） |
| `PWF_PAYMENT__ORDER_TTL` | 支付订单有效期（默认 `30m`，过期订单在下一次创建时关闭） |
| `PWF_ALERTS__WEBHOOK_URL` | 告警 Webhook 地址；留空则只落库、`notify_status=skipped` |
| `PWF_ALERTS__WEBHOOK_SECRET` | Webhook HMAC 密钥（`X-PWF-Webhook-Signature`，生产必须替换） |
| `PWF_ALERTS__WEBHOOK_TIMEOUT` | Webhook 单次投递超时（默认 `10s`，5xx/超时进入 job 重试） |
| `PWF_INVOICE__PROVIDER` | 发票适配器：`manual`（运营回录票号）。未知值启动即失败；生产允许 `manual` |
| `PWF_SCHEDULE__TIMEZONE` | 可用性规则、锁位 TTL 与提醒时区的解释时区（IANA 名称，默认 `Asia/Shanghai`；当前为租户全局单时区） |
| `PWF_SCHEDULE__HOLD_TTL` | 锁位有效期（默认 `30m`，必须为正；CI 用 `5s` 覆盖 Worker 过期回收） |
| `PWF_REMINDERS__ENABLED` | 是否规划/投递提醒（默认 `true`；关闭后 Worker 不再扫描到期提醒） |
| `PWF_REMINDERS__SHOOT_LEADS` | 拍摄前提醒提前量，逗号分隔（默认 `24h,2h`，必须 >0、≤720h，自动去重降序） |
| `PWF_REMINDERS__AUTH_LEAD` | 未完成授权提醒提前量（默认 `24h`；`0s` 表示停用该类提醒） |
| `PWF_REMINDERS__HOLD_LEAD` | 锁位到期提醒提前量（默认 `10m`；`0s` 表示停用） |
| `PWF_REMINDERS__WEBHOOK_URL` | 提醒 Webhook 地址；留空时站内通知仍可读、投递记 `skipped` |
| `PWF_REMINDERS__WEBHOOK_SECRET` | 提醒 Webhook HMAC 密钥（URL 非空时至少 16 字符；`X-PWF-Webhook-Signature`，生产必须替换） |
| `PWF_REMINDERS__WEBHOOK_TIMEOUT` | 提醒 Webhook 单次投递超时（默认 `10s`，5xx/超时进入 job 重试） |
| `PWF_GALLERY__PRESIGN_TTL` | 客户预览图的签名 URL 有效期（默认 `15m`，图片不经 API） |
| `PWF_GALLERY__SESSION_TTL` | 客户会话有效期（默认 `2h`，且不超过分享 token 本身的有效期） |
| `PWF_GALLERY__SELECTION_BATCH` | 单次选片批量的最大条数（默认 `200`） |
| `PWF_GALLERY__MAX_ASSETS` | 单相册最大资产数（默认 `5000`） |
| `PWF_GALLERY__WATERMARK_TEXT` | 默认水印文字；相册创建时快照，空则使用租户名 |
| `PWF_GALLERY__WATERMARK_FONT` | 水印字体文件（TTF/OTF/TTC；生产必填，中文需 Noto Sans CJK 等；空则内嵌 Latin 回退） |
| `PWF_GALLERY__REMINDER_LEAD` | 截止前提醒提前量（默认 `24h`；`0s` 停用） |
| `PWF_GALLERY__PUBLIC_BASE_URL` | 客户 H5 源，用于拼分享链接（生产必填且必须 https） |
| `PWF_PROOF__SESSION_TTL` | 审片客户会话有效期（默认 `4h`，且不超过审片 token 本身的有效期） |
| `PWF_PROOF__TOKEN_GRACE` | 审片 token 在 `due_at` 之后的宽限（默认 `168h`） |
| `PWF_PROOF__MAX_ASSETS` | 单轮审片最大资产数（默认 `500`） |
| `PWF_PROOF__ANNOTATION_MAX_BYTES` | 归一化批注 JSON 上限（默认 `16384`） |
| `PWF_INTEGRATIONS__PROVIDER` | 修图对接：`manual`（默认）或 `folder_watch`；`ftp`/`pixcake`/`xiangxiang` 为占位，选中即启动失败 |
| `PWF_INTEGRATIONS__FOLDER_WATCH__ROOT` | FolderWatch 共享根目录；选 `folder_watch` 时必填 |
| `PWF_INTEGRATIONS__FOLDER_WATCH__POLL_INTERVAL` | 回传目录轮询间隔（默认 `30s`） |
| `PWF_INTEGRATIONS__FOLDER_WATCH__OVERDUE_AFTER` | 修图任务超期告警阈值（默认 `72h`） |
| `PWF_NOTIFY__EMAIL__ENABLED` | 是否启用审片邮件（生产必须为 `true`） |
| `PWF_NOTIFY__EMAIL__HOST` / `__PORT` | SMTP 主机与端口（STARTTLS 常用 `587`，隐式 TLS `465`） |
| `PWF_NOTIFY__EMAIL__USERNAME` / `__PASSWORD` | SMTP 认证；用户名留空表示不鉴权（仅本地 sink） |
| `PWF_NOTIFY__EMAIL__FROM` | 发件人地址（启用邮件时必填） |
| `PWF_NOTIFY__EMAIL__TLS` | `starttls`（默认）/`tls`/`none`；生产拒绝 `none` |
| `PWF_NOTIFY__EMAIL__TIMEOUT` | SMTP 单次会话超时（默认 `20s`） |
| `PWF_NOTIFY__SMS__PROVIDER` | 仅接受空或 `sandbox`（只记录不真发）；生产拒绝 sandbox，未知值启动失败 |
| `PWF_DELIVERY__PUBLIC_BASE_URL` | 客户下载 H5 源；留空回退 `PWF_GALLERY__PUBLIC_BASE_URL`，生产设置时必须 https |
| `PWF_DELIVERY__SESSION_TTL` | 下载会话有效期（默认 `30m`，5m–24h，且不超过 token 有效期） |
| `PWF_DELIVERY__DEFAULT_TOKEN_TTL` / `__MAX_TOKEN_TTL` | 下载链接默认/最长有效期（`336h` / `720h`；签发 1–30 天） |
| `PWF_DELIVERY__PRESIGN_TTL` | 交付包与单张下载的签名 URL 有效期（默认 `5m`，1m–1h） |
| `PWF_DELIVERY__MAX_DOWNLOADS` | 单链接默认下载次数上限（默认 `20`；签发参数 1–100） |
| `PWF_DELIVERY__MAX_ASSETS` | 单次交付最大资产数（默认 `300`，上限 5000） |
| `PWF_DELIVERY__TEMP_DIR` | 打包临时目录（空用系统临时目录；需可写且容量足够） |
| `PWF_DELIVERY__WEB_EDGE` | web 包长边（默认 `2048`，512–4096） |
| `PWF_DELIVERY__REQUIRE_WEB_SPEC` | `true` 时任一资产无法生成 web 版即构建失败（默认 `false`，只写 warning） |
| `PWF_ARCHIVE__SWEEP_INTERVAL` | 归档补建与保留告警扫描间隔（默认 `5m`，1m–24h） |
| `PWF_ARCHIVE__DEFAULT_KEEP_DAYS` / `__DEFAULT_WARN_DAYS` | 租户默认保留/提前告警天数（`365` / `30`；owner 可在管理端覆盖） |

完整示例见 `.env.example`。`.env` 不会被自动加载，请用 `export`/`set -a; source .env` 或容器环境注入。

`make reset` 会 `docker compose down -v` 清空本地数据卷，仅在需要全新验证时使用。

MinIO 控制台：<http://localhost:9001>（`pwf` / `pwf-secret`），bucket `pwf` 由 API 启动时自动创建。

## 测试

```bash
make test            # 单元测试：密码/JWT/状态机/报价计价/合同与线索/电子签签名/支付投影与签名/发票状态机/变更单/告警去重与 HMAC/排期状态机与可用性/拍前准备校验与快照哈希/提醒规划与签名/采集校验/派生图与水印/LQIP/相册状态机与 token 哈希/manifest/客户会话/审片状态机与收费闸门/批注校验/评论幂等/修订校验和/派生/集成工厂/FolderWatch/邮件模板与投递/交付闸门顺序与 token 上限/ZIP 命名清洗/Store/manifest/校验和不符/临时文件清理/web 规格渲染/归档核验分类与保留策略校验
make agent-test      # Agent：SQLite 状态机与恢复、双写三方哈希、EXIF/预览、上传续传、清单验签、更新安装与回滚、NSIS 布局、Windows 长路径、gallery 预览与派生上传
make agent-e2e       # 无头 CLI：夹具导入双目的地 + 逐文件哈希比对 + 重复插卡拒绝 + selftest；设置 PWF_AGENT_E2E_SERVER 时追加真实派生上传断言
make web-test        # 两个前端：vue-tsc + vitest（含批注坐标/状态机/上传重试/交付错误映射与下载配额用例）
make web-build       # web-admin（含官网）与 web-gallery 生产构建
make e2e-gallery     # 独立栈 + Playwright：选片流程 + 审片流程（批注/提修改/接单/上传/回传/通过）+ 交付流程（构建/签发/包与逐张下载/撤销/邮件）+ mailsink 断言
make migration-matrix  # 迁移 0019→0018→…→0015 回滚、重新升级、0019/0018/0017/0015 有数据时拒绝回滚、worker 权限与列级行锁
make vet
make ci              # Docker 集成门禁：全新 Postgres+MinIO、迁移、seed、api+worker+mailsink、smoke（含发票/告警/排期/拍前准备/提醒/变更单/200 并发抢档/图库/审片/交付/归档全流程）
PWF_CI_MULTIPART=1 make ci   # 追加 40MiB 分片上传用例
PWF_CI_WEB=1 make ci         # 追加 web-admin（含官网）、web-gallery 与 Playwright 图库+审片+交付 E2E（CI integration job 默认开启）
```

Agent 安装包已在 Windows 实机验证：静默安装到自定义目录后 `current.txt`/版本目录/`tools`/快捷方式/HKCU 卸载项齐全，`uninstall.exe /S` 删除安装树、快捷方式与注册表项，并保留 `%LOCALAPPDATA%\PWF\agent` 用户数据；launcher 在 `current.txt` 切换后启动对应版本并透传参数。更新链（签名清单 → 校验下载 → 安装 → 回滚告警）由 `agent/internal/binder/update_test.go` 与 `cmd/pwf-release` 测试覆盖。

`make ci`（`scripts/ci.sh`）使用独立 compose 项目与端口（Postgres `55432`、MinIO `59000/59001`、HTTP `18080`、告警接收 `18081`、mailsink `18082`），不会碰本地开发栈；设置 `PWF_CI_AGENT=1` 时在 smoke 之后追加 Agent 无头导入 E2E（含真实服务端的派生上传断言，CI 默认开启），设置 `PWF_CI_WEB=1` 时再追加 `scripts/gallery-e2e.sh`（独立端口 `55434/59006/59007/18084/4175/4174`，mailsink `19025`；依次跑选片、审片与交付三段浏览器流程）。失败时 API/Worker/compose 日志落在 `deploy/.ci/`，图库 E2E 日志与截图落在 `deploy/.ci-gallery/`，mailsink 捕获的 `.eml` 落在 `deploy/.ci/mailsink/` 与 `deploy/.ci-gallery/mailsink/`。GitHub Actions：`.github/workflows/ci.yml` 分 `unit`（服务端 gofmt/vet/`go test -race`）、`agent-unit`（Agent gofmt/vet/`go test -race` + 前端 `vue-tsc`/Vite）、`web`（`web-admin` 官网/认证/管理端与 `web-gallery` 的 `npm ci`/typecheck/vitest/build）与 `integration`（`scripts/ci.sh` + 前端 E2E，失败上传日志 artifact）四个 job；`.github/workflows/agent-windows.yml` 在 `windows-latest` 上跑 Agent 单测、`selftest`、`wails build -tags wails`、无头导入 E2E 与 NSIS 打包，并单独运行 gallery 预览/派生上传测试（dev 未签名产物作为 artifact，路径过滤只在 `agent/**` 变化时触发）。

数据库相关验证同样可手工执行：先 `make up && make migrate && make seed`，再跑 `make api` + `make worker` + `make smoke`。

## 关键设计约束

- **tenant_id 从 day 1**：所有业务表带 `tenant_id`，查询显式过滤，RLS 双重防御。租户连接通过 `set_config('app.tenant_id', ..., true)` 开启。
- **Worker 独立账号**：API 使用 `pwf_app`，迁移使用 `pwf`，Worker 使用独立的 `pwf_worker` 连接；Worker 为 `NOBYPASSRLS`，只被授予 `jobs`/`assets`/`ingest_batches`/`outbox_events`/`payment_alerts`/`payment_orders`（只读 + 更新，供清扫）/`audit_logs`（只插）/`bookings`/`resource_blocks`/图库/审片/交付/归档相关表（各自迁移内最小授权，如 `archives` 无 DELETE、`projects` 仅列级 `UPDATE (id)` 供 finalize 行锁）的权限，不允许在线 Worker 使用迁移账号。
- **租约即所有权**：`jobs` 与 `outbox_events` 的每次终态写入都匹配 `locked_by + lease_token`；租约过期的行可以被其它 Worker 回收，旧持有者的结果一律丢弃（投递/任务语义为 at-least-once，不能双确认）。
- **素材去重靠内容哈希**：`UNIQUE (tenant_id, project_id, content_hash)`，重复卡/重复导入自动幂等。
- **设备台账与卡片指纹分离**：`agent_devices.fingerprint` 标识一台 Agent 安装（机器/安装身份），卡片指纹只出现在 `ingest_batches.card_fingerprint`；设备注册按 `(tenant_id, fingerprint)` 幂等，禁用是安全控制，重新注册不会复活设备或覆盖其公钥。公钥仅登记（ECDSA P-256 PKIX DER），M4 仍用用户 JWT，设备令牌留待后续版本。
- **字节不经过 API**：API 只发预签名 URL，Agent 直传对象存储。
- **对象 key 内容寻址**：`t/{tenant}/p/{project}/{hash[:2]}/{hash}/original{ext}`，同字节永远同 key。
- **状态机是唯一入口**：生命周期只允许前向推进；任何 active 状态可进 `on_hold`/`cancelled`，`on_hold` 只能回到中断前的状态。`contracted` 不再要求签署，`booked` 仍要求定金到账，`completed` 要求款清；合同与支付状态仍只由回调/专有服务写入，项目迁移不直接改。
- **金额一律 int64 分**，禁止浮点。
- **校验在 Worker 完成**：complete 只查对象大小；finalize 将批次置为 `verifying` 并投递 `ingest.verify` job，Worker 回读对象、按大小 + 流式 SHA-256 校验后才置 `verified`，批次收敛到 `completed`/`partial`/`failed`。校验是异步的，API 返回 202 + `job_id`，客户端轮询 `/agent/v1/jobs/{jobID}`。
- **写操作幂等**：所有创建/迁移/上传类接口支持 `Idempotency-Key`；键 + 请求体哈希落 `idempotency_keys`，重放直接返回首次响应。
- **事件不丢**：业务事务内同时写 `outbox_events`，Worker 负责投递（M0 仅记录并标记 sent，后续接消息总线）。
- **预签名有效期 1 小时**，分片会话在重试时复用，失败会标记 aborted 并在下次预签名时重建；封批后的批次拒绝新的 presign/complete。
- **报价快照不可变**：发送报价时冻结 `snapshot`（版本号、明细、客户名、计价结果），合同只引用快照；后续改价只能新开版本，旧 sent/accepted 自动 `superseded`。
- **合同状态只由服务写入**：创建/发送由后台 API 写，`signed`/`void` 只能由签名回调按合同当前状态推进；每次回调事件按 `(provider, event_id)` 唯一落 `esign_events`，重复投递幂等。
- **回调先验签再查租户**：webhook 不带 JWT，签名失败 401；通过后由 `esign_tasks`/`payment_order_routes` 反查 `tenant_id` 再开租户事务，避免回调被用于跨租户探测。
- **支付金额服务端推导**：发送报价时将冻结金额同步到项目台账；下单只收 `project_id` + `kind`，金额/币种来自项目台账与已入账净额，客户端不能指定；签署可选，尾款仍需定金已付。
- **支付只由回调或对账入账**：`payments` 行只能由验签通过的 `payment.succeeded` 或 `reconcile` 查询渠道后写入（合成事件 `reconcile:<out_trade_no>`）；`UNIQUE (order_id)` 与事件唯一键 `(provider, event_id)` 双重防重，金额/币种不符只记 `rejected` 与审计、不入账；已关闭订单的迟到支付只记 `flagged` 与 `payment.late_payment` 告警。
- **渠道状态可恢复**：下单先落 `creating` 预留再调渠道，关单先关渠道再关本地；`QueryOrder`/`CloseOrder` 与 `POST /payments/orders/{id}/reconcile` 覆盖“回调丢失”“关单后已支付”两类竞态，Provider 调用始终在事务外。
- **退款独立记账**：退款是独立 `refunds` 行，不修改原支付；`pending`/`succeeded` 都占用可退额度，失败后释放；项目状态用 `sum(payments) - sum(succeeded refunds)` 重算。
- **支付路径统一锁序**：project → order → payment → refund，Provider 调用在事务外，避免回调与退款并发时死锁或长时间持锁。
- **金额口径分离**：核心应收来自项目台账；商务页会在发送报价时把冻结金额同步到台账（合同存在时也冻结同一报价快照；`deposit`/`balance`/`full` 三种订单）；变更应收独立投影到 `projects.change_amount_cents`，只由变更单聚合写入。有效总额 = 核心报价金额 + 变更总额，`net` 含全部收款、`coreNet` 只含核心订单收款；`balance` 订单金额永远不含变更金额，避免“补款单 + 尾款单”重复收款。`change_pending` 表示核心已清、变更未清，`booked` 放行、`completed` 仍需 `paid`。
- **结算钩子同事务**：发票红冲与变更单生效挂在 `payment.OrderHook`/`RefundHook` 上，在订单置 paid / 退款置 succeeded 之后、项目投影刷新之前调用；钩子失败整个回调事务回滚，渠道重试，绝不出现“钱入账但变更未生效”的中间态。
- **发票金额不可篡改**：申请时金额取自支付行；登记时重新 `Stat` 对象并流式 SHA-256 校验，客户端提交的哈希只是期望值。红冲累计不超过发票金额，`refund_id` 唯一兜底重复回调。
- **变更单必须签署且独立收款**：草稿金额随行项目计算；`esign_tasks` 按 `contract_id`/`change_order_id` 二选一分发回调；签署成功才允许开补款单，支付成功才计入应收；取消先关渠道、拒绝已支付取消，全额退款自动反转。
- **告警不阻塞主事务**：告警写入包在 savepoint 中，投递失败只影响通知状态，不影响支付回调的入账结果；去重靠部分唯一索引，重复事件只累加 `occurrences`。
- **档期仲裁只认数据库**：所有占用（锁位、确认预约、手工屏蔽/闭店）都写 `resource_blocks`，由 GiST 排他约束 `(tenant_id, resource_id, during)` 裁决；应用层预检只做友好提示，并发下唯一能改变结果的仍是数据库。容量恒为 1，器材按物理件建模。
- **锁位与确认分离**：创建预约只锁档（TTL 内有效），确认要求定金/全款已付，不要求合同签署；支付结算钩子自动确认未过期的 held 预约，锁位已过期则保留款项并告警 `booking_hold_expired`，不静默占用档期。
- **单项目单活跃预约**：部分唯一索引 `(tenant_id, project_id) WHERE status IN ('held','confirmed')` 保证项目拍摄时间与预约一一对应；确认/改期同步 `projects.shoot_start_at/end_at`，取消时同事务将项目置为 `cancelled`，confirmed 且时间匹配时清空拍摄时间。
- **取消不退款**：取消与改期只释放/迁移占位并写审计，资金动作必须显式走退款接口，避免排期操作隐式触发资金流。
- **场次即预约**：拍前准备不引入独立 `shoots` 表，shot list/器材/授权/call sheet 全部挂在 `booking_id`；改期自动跟随，取消/过期后只读保留历史。多场次项目仍需拆项目，这是既有单活跃预约约束的延伸。
- **拍前写窗口**：只在 `held/confirmed` 接受新增与修改（call sheet 仅 `confirmed` 可冻结）；`cancelled/expired` 一律 409 `illegal_state`，不删除任何历史。
- **call sheet 快照不可变**：冻结时以 Go 结构体序列化为规范 JSON（空集合为数组、UTC 时间），按精确字节存 `text` 并记录 SHA-256；读取时重算哈希，不一致直接失败。改期不回溯旧版本，新通告必须冻结新版本。
- **HTML 只从冻结快照渲染**：`html/template` 自动转义，`reference_url` 限 http(s)，无 JS/外部资源，并下发 `no-store`/`nosniff`/严格 CSP。
- **授权证据不可删**：`signed_at` 由服务端写入，`waived` 必须备注；`signed/waived` 行只能更正、不能删除，删除只允许 `pending`。
- **提醒规划不阻断业务**：提醒重排包在 savepoint 中，失败只记日志；去重键包含 booking 与时间，只有 pending 行占用唯一键，改期后旧提醒被取消；Worker 投递前复核状态，过期/已改期/授权已完成的提醒一律 `skipped`，绝不发送过期内容。
- **Webhook 签名只有一份实现**：`internal/webhook` 提供 `t=,v1=` 签名/验签，告警与提醒共用，避免多份 HMAC 代码漂移。
- **分享 token 只在 fragment**：链接是 `/#/a/<token>`，浏览器不会把 fragment 发给服务器；API 只接受 `POST /g/v1/session` 的交换请求，日志与 `Referer` 都不含明文 token，库里只存 SHA-256。
- **图片字节不经过 API**：客户接口只签发最长 15 分钟的 GET URL；会话和 URL 都按请求回查撤销状态，撤销 token/相册后旧链接立即失效且不再签发新 URL。
- **发布是同步闸门**：publish 校验全部 thumb+display 就绪，缺一张就 409 `derivatives_incomplete`；通过后 manifest 是不可变对象，资产集合冻结，改选必须新轮次。
- **选片版本独立于相册版本**：客户写选片走 `selection_version`（每次批量 +1），管理端改设置走 `galleries.version`；两者互不干扰，重复提交幂等。
- **水印在 display 上、thumb 无水印**：列表要快，灯箱要防外流；水印文字在发布前快照，字体必须覆盖文字（生产强制配置字体），不允许静默输出方框。
- **派生失败不阻塞采集**：gallery 预览上传失败只记派生事件，原图与批次照常完成；缺派生在发布时显式拦截，绝不把未加水印的图发出去。
- **审片会话是第三套命名空间**：`pwf-proof` 与员工 JWT、选片会话互不通用；每次请求回查 token/轮次/相册，撤销即时生效；审片只认相册未撤销，忽略选片截止。
- **每轮只允许一次修图往返**：`resubmitted` 只能通过，再提修改必须新建轮次（触发包含轮次与收费闸门）；这是产品约束，避免客户无限返修绕过收费。
- **额外轮次先付费**：`round_no > contract.included_rounds` 必须挂同项目已生效、含 `extra_retouch` 项的变更单，一单只用一次（部分唯一索引 + DB 触发器兜底）；owner override 必须填原因并写审计。
- **修图版本内容寻址**：同一资产同一 SHA-256 重复导入返回旧版本（200 `duplicate`），声明校验和与实算不一致直接拒绝；`revision_no` 递增且不可覆盖，原图派生与修图派生互不覆盖。
- **邮件不长期留存链接**：正文含一次性审片 token，发送成功即清空 `email_messages.body_text`；发送前复核轮次/相册仍可访问，失效则 `skipped`，不发出死链。
- **批注与分辨率解耦**：只存 0–1 归一化坐标，原图与任意修图版本共用；服务端严格校验形状与大小，非法批注 422。
- **FolderWatch 只做单向交换**：导出源文件、回传成品 JPEG/PNG/WebP，已处理文件移入 `done/`、坏文件留 `failed/`；按 SHA-256 幂等，不做双向冲突合并。
- **尾款闸门没有后门**：构建、签发、会话与每次下载都实时读 `projects.payment_status`；退款回落后已签发链接在下一次请求即 409 `payment_required`，owner 也没有 override。
- **交付包可复现**：ZIP entry 不压缩、时间固定 1980-01-01、文件名确定性清洗；`manifest.json` 记录每项来源与 SHA-256，原图打包时边复制边比对，源对象被替换会让构建失败而不是交付坏文件。`entry_count` 只统计交付文件（不含 manifest）。
- **web 与原图分离**：web 包长边 2048、无水印，用于快速查看/转发；原图包字节原样（有审片通过的修图版时用修订源替换）。HEIC/RAW/TIFF 只进原图包并写 warning，`require_web_spec=true` 时才整单失败。
- **下载链接是第四套命名空间**：`pwf-delivery` 会话与员工 JWT、选片会话、审片会话互不通用；token 只存 SHA-256，撤销/退款每次请求生效；`max_downloads` 统计授权次数，预签名 URL 在 5 分钟 TTL 内可复用属已知边界。
- **构建幂等且可收敛**：`build_version` 贯穿全部 job，旧版本任务 no-op；包任务重试会再次执行 finalize；finalize 在 `FOR UPDATE OF deliveries … FOR SHARE OF projects, proof_rounds` 下重查支付、项目状态与轮次，杜绝“退款后仍发布”。Worker 对 `projects` 只有列级 `UPDATE (id)`，只够拿行锁、不够改数据。
- **归档只校验、不删除**：逐对象 SHA-256 校验，缺失/篡改明确落 `missing`/`mismatch` 并告警；保留到期前只提醒一次，从不自动删除对象。
- **删除是单向门**：owner + `confirm:"DELETE"` 字面量 + 原因 + 审计，逐对象删除后保留墓碑行；合同/支付/发票/审计等数据库行永不随对象删除。

## 已知残余风险

- **真实渠道适配器未实现**：`payment.Provider`/`esign.Provider` 目前只有 `sandbox`；微信支付/支付宝/e签宝等适配器需要商户凭据与厂商签名规范。工厂对已知厂商名直接报“未实现”、对未知名称报未知，`env=production` 还会拒绝 sandbox（含留空默认）与示例密钥；因此 `env=production` 在本仓库必然启动失败，这是刻意的上线门禁，接入真实适配器并验证后才允许切换。接入时必须同时满足：`CreateSignTask` 按合同 ID 幂等、实现 `TaskCanceller`、回调验签与重放窗口经厂商测试环境验证。
- **真实发票厂商适配器未实现**：`invoice.Provider` 只有 `manual`，票号/代码由运营回录；数电票 API 接入时实现同一接口即可，现有状态机、文件校验与红冲逻辑无需改动。
- **不支持部分开票**：v1 限定一笔支付一张有效发票、金额等于支付金额；分期开票与客户自助申请留待后续版本。
- **渠道账单下载/批量对账未实现**：当前对账是单订单 `QueryOrder`，只解决回调丢失；日终账单核对（`DownloadBill`）与差异工单留待真实渠道接入时补齐。
- **合同发送已收敛、变更单未收敛**：合同采用 `submitting` 预留 + 幂等建单 + 撤销补偿，落库失败不会留下不可追踪的渠道任务；变更单的 `Send` 仍是“先调渠道、后落库”，接入真实厂商前需按合同同样改造。另外，厂商任务在本地落库前若回调先到，会因查不到 `esign_tasks` 映射被 `ignored`，依赖厂商按规范重试回调。
- **`creating` 预留在沙箱下会被本地关单**：Worker 清扫调用 `QueryOrder`，sandbox 订单状态只存在于 API 进程内存中，Worker 看不到时按“渠道无单”关单——这在 sandbox 下是正确恢复，真实渠道以厂商查询结果为准。清扫间隔 60 秒、单轮 50 单，积压时可调大 `sweepPaymentOrders` 的批次。
- **告警 Webhook 无签名重放窗口限制**：接收方需按 `timestamp` 自行做时间窗校验（与签署回调一致），重试上限 5 次后标记 `dead`，需人工介入。
- **排期容量恒为 1**：不建模“多机位/多助理同时段/器材数量库存”，需要并行资源时建多条资源记录；真正的库存与容量模型留待业务明确后扩展。
- **单项目单活跃预约**：一个项目同时只能有一条 held/confirmed 预约，多场次（如婚前 + 婚礼当天）需拆项目；该限制由部分唯一索引硬约束。
- **排期时区为租户全局**：`PWF_SCHEDULE__TIMEZONE` 对整个部署生效，跨地区租户需改为 per-tenant 时区并重算规则解释。
- **锁位过期/项目取消但已付款需人工退款**：自动确认只接受未过期且项目仍可服务的锁位；过期或项目已取消时支付成功会保留款项并产生 critical 告警（`booking_hold_expired` / `booking_confirm_blocked`），退款走既有退款接口。
- **站内通知是租户级共享已读**：M3 没有收件人模型，任何人标记已读即对全租户生效；未读数也是租户级。需要按人分发时再加 `notification_recipients`。
- **提醒只有站内 + Webhook**：未接邮件/短信/IM 厂商，Webhook 未配置时投递记 `skipped`，只能靠应用内查看；接收方需自行处理重试与幂等。
- **授权无附件**：授权台账只记录类型/对象/状态/时间/备注，签署文件上传与存证留待后续；call sheet 快照不含授权备注，避免证据细节扩散。
- **改期需重新冻结 call sheet**：旧版本永远保留原时间与哈希，不会自动重排；团队必须按流程冻结新版本并分发。
- **提醒历史无清理策略**：`notifications`/`notification_deliveries` 长期累积，生产需加归档或定时清理。
- **多场次项目需拆项目**：booking 即场次且单项目单活跃预约，婚前 + 当天等多场次场景目前必须拆成多个项目。
- **设备禁用暂无管理接口**：M4 只提供注册与心跳，`disabled` 需运营直接改库；设备公钥仅登记，认证仍用用户 JWT，设备令牌落地前注册/心跳的权限不高于该用户。
- **Agent 更新依赖发布方保管私钥**：清单 Ed25519 公钥烧进客户端，轮换公钥必须发新版本（旧版本无法验证新密钥）；私钥泄露等价于可向所有客户端分发任意安装包，需按密钥事件响应。stable 还依赖代码签名证书，证书过期/更换签名者指纹时必须同步更新 `BuildSignerThumbprint`。
- **回滚只覆盖启动失败**：连续两次启动未到达健康状态才切回上一版本；运行期崩溃（如导入大卡时 OOM）不计入，需用户手动重试或卸载重装。
- **WebView2 为外部依赖**：未随包捆绑 bootstrapper 的安装包要求系统已安装 Evergreen Runtime；Windows 10 1809 以下直接拒绝安装。
- **图片与交付包分发尚未接生产 CDN**：M5/M6/M7 都只用 MinIO 预签名（预览 15 分钟、交付 5 分钟），CloudFront/阿里云等签名适配器是上线门禁；切换时需保证客户接口仍只返回短期 URL、API 不代理字节，并实现 `NamedPresigner`（附件文件名）或接受文件名退化。
- **水印不是 DRM**：display 图带文字水印、可阻止直接外发，但屏幕截图/翻拍无法防止；更高保护（平铺水印、指纹追责）留给交付阶段评估。
- **CJK 水印依赖部署字体**：内嵌回退只有 Latin，生产必须挂载 Noto Sans CJK 等字体并设置 `PWF_GALLERY__WATERMARK_FONT`；缺字体启动直接失败，避免静默输出方框。
- **客户邮件依赖发件域信誉**：M6 已实现 SMTP 入队/重试/发送后清空正文，但送达率取决于 SPF/DKIM/DMARC 与发件域配置；未实现退信/投诉回执处理，最终失败只置 `dead` 并告警，需人工跟进。
- **短信只有 sandbox**：真实短信厂商适配器未交付，生产拒绝 sandbox，管理端 `/notify` 选 sms 且未配置时 409；启用前需新增 Provider 实现。
- **FTP/PixCake/享像派仅接口占位**：选中这些 provider 会启动失败（含生产），必须先接适配器；不会静默回退到 manual。
- **FolderWatch 依赖目录稳定性**：断链、权限、同名文件会表现为延迟或告警；只做单向文件交换，不做双向冲突合并，且 `incoming/` 只接受 JPEG/PNG/WebP（RAW/TIFF/PSD 留待后续）。
- **每轮仅一次修图往返**：客户对 `resubmitted` 仍不满意时必须新建轮次（可能触发额外收费/owner override），这是产品约束而非缺陷。
- **无修图师分配模型**：`retoucher` 只读，owner/staff/assistant 均可接单与上传；按人分派与工时统计留待后续。
- **归档不自带对象生命周期**：归档只做校验、保留告警与 owner 审批删除，不自动清理对象；长期存储成本请配置 bucket lifecycle，并把 owner 删除纳入常规演练。
- **邮件重试窗口内正文仍在库**：发送成功才清空正文；pending/failed 行在重试期间仍包含一次性链接，需按数据库访问权限与备份策略控制暴露面。
- **限流与 manifest 缓存是单实例内存**：客户选片、审片与交付路由的固定窗口限流、manifest 短缓存都只在本进程内；多副本部署需要共享限流（如 Redis）或网关限流。
- **旧 RAW 资产需补传预览**：发布只接受拥有 gallery 源的资产；升级 Agent 前导入的 RAW 需重新导入/补传，服务端只能为 JPEG/PNG/WebP 直接派生。
- **预签名 URL 在 TTL 内可复用**：`max_downloads` 统计的是签发授权次数，不是字节传输次数；拿到 URL 的人在 5 分钟内可重复下载。若要严格单次下载需改为 API 流式代理，与“字节不经过 API”原则冲突，当前以短 TTL + 可撤销 token + 审计控制。
- **交付只绑定已通过的审片轮次**：没有审片流程的项目无法创建交付；客户“不审片直接要成片”的场景需要后续增加“直接交付”路径。
- **HEIC/RAW 没有 web 版**：默认只保证原图包可下载（web 包跳过并写 warning）；`require_web_spec=true` 时构建会整单失败。
- **web 渲染有源上限**：单文件超过 80 MiB 或无解码器时跳过 web 版；原图包不受影响。
- **归档删除不可逆且无软删除**：依赖 owner 审批、`DELETE` 确认、审计与备份/对象版本化兜底；误删只能从对象存储版本或备份恢复。
- **归档逐对象 job 有队列压力**：大项目会产生成百上千个校验/删除任务，靠批次与 5 分钟 sweep 限流；生产建议为归档单独部署 Worker 池，避免与交付/邮件任务互相挤占。

## 技术取舍

- 任务队列当前是数据库表 `jobs`（`FOR UPDATE SKIP LOCKED` + 退避 + 死信），未引入 Asynq/Redis。理由是 M0 任务量与并发都低，DB 队列少一个运维组件且与业务事务同源；等 M4 派生图任务出现 CPU 密集并发后再评估 Asynq。
- 指标为手写 Prometheus 文本输出（`httpx.NewMetrics`），未引入 client_golang，避免 M0 增加依赖。

## 下一步

1. 上线门禁：按 `docs/runbook-delivery-archive.md` 与 `docs/dr.md` 完成一次演练（压测、支付重放、PITR/对象恢复），并把结果存档到发布记录。
2. 渠道接入：支付侧接入微信支付 v3/支付宝 RSA2（实现 `payment.Provider`，含日终对账），电子签接入 e签宝（幂等建单 + `TaskCanceller` + 回调验签），图库/审片/交付侧接入 CDN 签名（CloudFront/阿里云，含 `NamedPresigner`）。
3. M6 收尾：真实短信 Provider（当前仅 sandbox）、FTP/PixCake/享像派适配器、修图任务按人分派。
4. M2 收尾：数电票厂商适配器、渠道账单批量对账；变更单发送的 `submitting` 补偿与合同对齐。
5. M4 现场验收：UNC、真实拔卡、磁盘满与长路径浸泡；发布候选包（安装包与更新链已可用）。
6. M7 收尾：对象存储生命周期（bucket lifecycle）与归档删除演练、交付/归档独立 Worker 池、客户“不审片直接交付”路径评估。
