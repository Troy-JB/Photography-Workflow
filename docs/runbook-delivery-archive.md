# 交付与归档故障处置手册（M7）

适用版本：迁移 `0019_delivery_archive.sql` 及之后。读者：值班工程师、owner、客户成功。

本手册只描述**可执行**的排查与处置动作。任何会删除对象、撤销链接、回滚迁移的操作都必须先确认影响面，并在审计中留下原因。

## 1. 前置信息

| 项 | 位置 |
|---|---|
| API / Worker | `server/cmd/api`、`server/cmd/worker` |
| 任务队列 | `jobs` 表（`kind`：`delivery.build` / `delivery.render_web` / `delivery.package` / `archive.verify` / `archive.verify_item` / `archive.delete_item`） |
| 审计 | `audit_logs.action`，本模块前缀 `delivery.` / `archive.` / `retention_policy.` |
| 告警 | `payment_alerts.code`：`delivery_build_failed`、`archive_verify_failed`、`archive_delete_failed`、`archive_delete_skipped` |
| 交付链接 | 明文只在签发/通知时出现一次；库中只有 `download_tokens.token_hash`（SHA-256） |
| 状态机 | 交付：`pending → building → ready｜failed｜blocked`；归档：`pending → verifying → ready｜failed → deleting → deleted` |

只读查询示例（使用迁移账号，避开 RLS）：

```bash
export PWF_DB_URL='postgres://pwf:...@host:5432/pwf'
psql "$PWF_DB_URL" -c "
SELECT d.id, d.status, d.build_version, d.item_count, d.blocked_reason, d.last_error,
       p.code AS project_code, p.payment_status, p.state,
       r.status AS round_status
FROM deliveries d
JOIN projects p ON p.tenant_id = d.tenant_id AND p.id = d.project_id
JOIN proof_rounds r ON r.tenant_id = d.tenant_id AND r.id = d.proof_round_id
WHERE d.tenant_id = '<tenant-uuid>'
ORDER BY d.created_at DESC LIMIT 20;"
```

## 2. 交付构建

### 2.1 构建失败（`delivery_build_failed`）

1. 看交付行：`SELECT status, build_version, last_error FROM deliveries WHERE id = ...;`
2. 看包行：`SELECT spec, version, status, last_error FROM delivery_packages WHERE delivery_id = ... ORDER BY version DESC, spec;`
3. 看任务：`SELECT id, kind, status, attempts, last_error FROM jobs WHERE ... ORDER BY created_at DESC LIMIT 20;`
4. 常见原因与处置：

| last_error 关键字 | 含义 | 处置 |
|---|---|---|
| `original_checksum_mismatch` | 源对象与登记的 SHA-256/大小不一致（对象被替换或损坏） | 停止重建；按 DR 手册恢复源对象后再建；不得跳过校验 |
| `web_spec_decode_failed` | 源文件无法解码为图片 | 默认只影响该条目的 web 规格（记 warning）；若租户开启 `require_web_spec`，需换源或关闭该开关后重建 |
| `web_spec_unsupported_mime` | HEIC/RAW/TIFF 等无 web 版 | 正常行为；客户只能下载原图。需网页版时先补 JPEG/PNG 预览 |
| 存储 5xx / 超时 | 对象存储不可用 | 恢复存储后重建，构建按版本幂等 |
| `no space left on device` | `delivery.temp_dir` 容量不足 | 扩容或调整 `PWF_DELIVERY__TEMP_DIR`；确认无残留 `pwf-delivery-*` 临时文件 |

5. 修复后重建：管理端「重新构建」或 `POST /api/v1/deliveries/{id}/build`。重建会递增 `build_version`，旧版本的包对象与行会被清理（保留上一版排障）。
6. 若项目已退款/未付款，构建与重建都会被闸门拒绝（`409 payment_required`），交付会进入 `blocked`。**没有 owner 覆盖开关**。

### 2.2 交付长期停在 `building`

- 检查 worker 是否在跑、队列是否有积压（`SELECT status, count(*) FROM jobs GROUP BY status;`）。
- 两个包都已 `ready` 但交付未发布，说明 finalize 没跑完：重建一次即可收敛（finalize 幂等）。
- 若 finalize 报 `permission denied for table projects (SQLSTATE 42501)`：`pwf_worker` 缺少列级 `UPDATE (id) ON projects`。核对迁移 0019 是否完整执行：

```sql
SELECT count(*) FROM information_schema.role_column_grants
WHERE grantee='pwf_worker' AND table_name='projects' AND column_name='id' AND privilege_type='UPDATE';
-- 期望 1
```

### 2.3 构建报 `build_in_progress`

同一交付已有进行中的构建。等待其结束；不要并发重建；重复点击是安全的（返回 409）。

## 3. 下载授权与尾款闸门

闸门顺序：撤销 → 过期 → 项目状态 → 尾款 → 审片通过 → 交付就绪。任何一项不满足都会即时生效，不依赖会话过期。

### 3.1 客户看到「尾款尚未结清」（`409 payment_required`）

- 确认 `projects.payment_status`。只接受 `paid`；`deposit_paid`/`refunded`/`unpaid` 都会拒绝。
- 退款后已签发的链接会在下一次请求立即失效；重新收款后，**未被撤销**的旧链接会恢复可用，无需重新签发。若要主动作废旧链接，用「全部撤销」。
- 不允许 owner 强制放行；如业务确需交付，先完成收款流程。

### 3.2 客户看到「下载次数已用完」（`409 downloads_exhausted`）

- `SELECT label, max_downloads, downloads, revoked_at FROM download_tokens WHERE delivery_id = ...;`
- 计数是**授权次数**：每次 `POST /g/v1/delivery/downloads` 消耗 1 次；预签名 URL 在 5 分钟 TTL 内可重复下载，不重复计数（已知边界）。
- 处置：签发新链接并发给客户；旧链接可保留或撤销。

### 3.3 下载链接泄露

1. 立即在管理端「全部撤销」，或 `POST /api/v1/deliveries/{id}/tokens/revoke-all`。
2. 审计排查：`SELECT created_at, actor_id, action, metadata FROM audit_logs WHERE subject_id = '<delivery-id>' ORDER BY created_at DESC;`，重点看 `delivery.token.created`、`delivery.download.granted` 的 IP 与时间。
3. 记录 `download_tokens.last_used_ip` / `last_used_at`。
4. 重新签发新链接并通知客户；在事故记录中注明撤销时间。
5. 注意：撤销前已经发出的预签名 URL 最长还有 5 分钟有效期（`delivery.presign_ttl`）。需要更严格时可临时调小该值并重启 API。

### 3.4 客户打不开链接（`410 link_expired` / `link_revoked` / `delivery_archived`）

- `link_expired`：token 超过 `expires_at`；重新签发。
- `link_revoked`：按 3.3 处置或确认是正常撤销。
- `delivery_archived` / `delivery_cancelled`：项目已归档或取消，交付冻结；归档项目不会恢复下载（可联系 owner 按归档策略处理）。

### 3.5 下载域名 / 存储异常

- 预签名来自 `storage.endpoint`；CDN 只应缓存公开资产，**不得缓存** `/g/v1/*` 响应（已下发 `no-store`）。
- 若下载 403/404：核对对象是否存在（`mc stat`/控制台）、bucket 策略、系统时间是否漂移（签名过期）。

## 4. 归档

### 4.1 核验失败（`archive_verify_failed`）

1. `SELECT status, object_count, verified_count, failed_count, last_error FROM archives WHERE id = ...;`
2. 失败项：`SELECT kind, object_key, status, note FROM archive_items WHERE archive_id = ... AND status IN ('missing','mismatch');`
3. `missing`：对象不存在（被误删/迁移遗漏）。先从备份/版本化恢复对象；`mismatch`：内容被改写，按备份覆盖并复核业务影响。
4. 恢复后重试：`POST /api/v1/archives/{id}/verify`。核验按对象幂等，只重跑未通过项。
5. 核验失败**不会**删除任何对象，也不会阻断客户已有下载（归档项目除外——项目归档会关闭下载闸门）。

### 4.2 保留到期告警（`archive_retention_warning`）

- 到期只发告警，**不自动删除**。
- 保留策略（keep/warn）只作用于新归档；已有归档沿用创建时的快照值，修改策略不会改写历史归档的 `retention_until`。
- 续期方式：对归档重试核验（`POST /api/v1/archives/{id}/verify`），核验完成后 `retention_until` 会重置为「核验时间 + 快照 keep_days」。`warned_at` 不会被清空，因此同一归档不会重复告警；若需要重新进入告警周期，需人工评估后处理。
- 到期状态查询：`SELECT id, project_id, retention_until, warned_at FROM archives WHERE status='ready' AND retention_until <= now();`

### 4.3 归档删除审批（不可逆）

仅 owner 可执行，且归档必须 `ready`、项目必须 `archived`。

1. 备份确认：数据库已保留（合同/付款/发票/审计行永不随对象删除）；对象存储已开启版本化或已做异地备份。
2. 管理端「归档详情 → 删除归档文件」，或：

```bash
curl -sS -X POST "$API/api/v1/archives/$ARCHIVE_ID/deletion" \
  -H "Authorization: Bearer $OWNER_TOKEN" -H 'Content-Type: application/json' \
  -d '{"confirm":"DELETE","reason":"客户确认删除，工单 #1234"}'
# 202；状态 deleting，逐对象任务执行后 deleted
```

3. 审计：`archive.deletion_requested`、`archive.delete_item`、`archive.deleted`；归档行保留为墓碑（`deleted_at` 非空）。
4. 若出现 `archive_delete_skipped`：对象仍被其他归档引用（当前模型下不应发生），先人工确认再处理。
5. 恢复：只能从对象备份/版本化恢复；数据库墓碑行不会被自动重建为 `ready`，需人工评估后走核验流程。

### 4.4 归档任务积压

- `archive.verify_item` 每个对象一条任务，会占用队列。可通过 `WORKER__BATCH_SIZE` 或独立 worker 实例分流；不要为了清空队列而删除 `jobs` 行（会丢审计）。
- 归档创建对同一项目幂等（`UNIQUE(tenant_id, project_id)`），重复调用安全。

## 5. 回滚

- **应用回滚**：先回滚 API（保留 0019 表），再回滚 worker。旧版本不会写新表，可安全共存。
- **迁移回滚**：0019 down 在 `deliveries`/`archives` 有数据时会直接拒绝（`delivery or archive tables contain rows`）。这是保护：不要为了回滚清空表。需要回滚时先导出归档数据并走变更审批。
- 回滚后交付/归档功能不可用，但客户已下载的文件与对象不受影响。

## 6. 上线检查清单

- [ ] `PWF_ENV=production`，支付/电子签使用正式适配器；sandbox 适配器在生产启动即被拒绝。
- [ ] `PWF_DELIVERY__PUBLIC_BASE_URL` 为 https（留空回退 `PWF_GALLERY__PUBLIC_BASE_URL`）。
- [ ] Worker 的 `temp_dir` 可写，容量 ≥ `max_assets × 单文件大小`；无历史 `pwf-delivery-*` 残留。
- [ ] `delivery/archive` 相关迁移已执行到 0019；`migration-matrix` 绿灯。
- [ ] 对象存储开启版本化；备份与 PITR 已验证（见 `docs/dr.md`）。
- [ ] 归档保留策略已按租户配置（默认 365/30）。
- [ ] 压测与支付重放只在测试环境执行，记录结果（`bash scripts/loadtest.sh`、`bash scripts/payment-replay.sh`）。隔离环境可一条命令完成（`bash scripts/launch-drill.sh` 或 `make launch-drill`）：自动起栈、造待支付订单、支付/退款各重放 200 次、普通 API 压测与 200 并发抢档，JSON 报告写入 `deploy/.drill/`。
- [ ] 交付/归档告警 webhook 已接入值班渠道；`delivery_build_failed`、`archive_verify_failed`、`archive_retention_warning` 均能到达。

## 7. 演练记录模板

```
演练名称：交付链接泄露处置 / 归档核验失败恢复 / 归档删除审批
日期与参与人：
演练环境与版本（commit、迁移版本）：
触发条件（告警 code / 客户工单）：
处置时间线（发现 → 止血 → 根因 → 恢复 → 复盘）：
验证证据（审计 action、下载记录、核验汇总、截图/日志路径）：
暴露的问题与改进项（负责人、截止日期）：
```

部署与迁移相关演练的灾备部分见 `docs/dr.md`。
