# CIOS Spec 008 — Operations（Ticket / PM / Capacity）

> 版本 **v0.4（Ticket L69；PM/Capacity/Reporting/Runbook/CMDB-Ops L70；Spare/Inspection/容量多维/对账扫描/工单备注·审计·版本·去重 + `<source>:<id>` 命名空间 as-built 冻结 L71，2026-06-21）**｜架构者起草 2026-06-19｜依赖：spec-001 §2（路径/crn）、spec-002（quantity）、spec-003（severity/状态机/CloudEvents profile/runbook 字段）、spec-004（API 约定 / RFC 7807 / 分页 / Scoped RBAC）、spec-006 §2.2（NATS subject）
> 分期：**v0.1/0.2 = Ticket（PRMT-032–034）**；**v0.3 = PM/Capacity/Reporting/Runbook/CMDB-Ops as-built（PRMT-039/040/042/043/044/045）**；**v0.4 = Spare（§16，PRMT-048/054）/ Inspection（§17，PRMT-049/059/063）/ alarm_id 命名空间（§18）+ Capacity cooling·rack·exporter（§10，PRMT-055/056/062）+ Reporting 对账扫描·保留（§11，PRMT-050/057/064）+ Runbook 来源链·检索（§12，PRMT-047/053）+ CMDB 检索·批量（§13，PRMT-067/068）+ Ticket 备注·审计·版本·去重（§1，PRMT-060/061/081/082）**（code-wins-once-tested）。
> 覆盖：`docs/M2-COMPLETION-PLAN.md` §6 四开放决策（a–d）；M2 退出 §M2-1（告警→工单闭环）/§M2-2（MTTR/MTBF）/§M2-3（容量决策）/§M2-4（月报自动）。
> **本规范与 PRMT-032–074 的契约一致**：spec 是权威源，PRMT 是其零发散实现。
> **✅ Q1–Q5（L69）；Q6–Q12（L70）；Q13–Q19（L71，2026-06-21，as-built 追认）**——下文措辞一律按**已决**读；详见 §19 + `docs/LOCKED.md` L69/L70/L71。
> **注**：PRMT-082（工单乐观版本 CAS）handler 接线**已落地**（commit `09a6967` 075–084 硬化批，architect static-PASS）；v0.4 全段 as-built。

---

# 1. Ticket 资源模型（E2.3）

Ticket = 一条运维工作项。M0/M1 无 ticket；M2 引入。持久化于 `core.Store`（file + pg 双实现，spec-004 §存储语义），PG 表 `tickets`（migration 002）。

## 1.1 字段

| 字段 | 类型 | 必填 | 语义 |
|------|------|------|------|
| `id` | string `tk_<16×base32>` | ✓ | 全局唯一（80-bit 随机）；`tk_` 前缀便于日志辨识（§8 / Q4 / L69）|
| `alarm_id` | string | — | 关联告警 id（自动开单时 = `alarms.id`，可 join）；手动工单为空 |
| `asset_path` | string (crn) | ✓ | 关联资产路径（spec-001 §2）；RBAC scope 过滤维度 |
| `title` | string | ✓ | 摘要（自动开单 = 告警 summary）|
| `severity` | enum | ✓ | `critical\|major\|minor\|info`（**复用 spec-003 §2**，不另立枚举）|
| `state` | enum | ✓ | `open\|acknowledged\|resolved\|closed`（§2 / Q1）|
| `assignee` | string | — | 处理人身份（spec-004 principal）|
| `opened_at` | timestamptz | ✓ | 创建时刻（NOT NULL）|
| `acked_at` | timestamptz | — | 进入 acknowledged 时刻（nil=未到）|
| `resolved_at` | timestamptz | — | 进入 resolved 时刻 |
| `closed_at` | timestamptz | — | 进入 closed 时刻 |

> nullable 时间戳用 `*time.Time`（Go）/ `TIMESTAMPTZ NULL`（PG）。表/索引见 migration 002（`tickets_alarm_id_idx`、`tickets_asset_path_idx`）。

## 1.2 不变量
- `id` 不可变；`opened_at` 不可变。
- 时间戳单调：`opened_at ≤ acked_at ≤ resolved_at ≤ closed_at`（存在的那些）。
- `severity` 创建后可改（手动重定级）；`state` 仅经 §2 合法转换变更。

## 1.3 备注 / 审计 / 乐观版本 / 去重（v0.4，PRMT-060/061/081/082）

- **备注（notes，PRMT-060，as-built）**：`ticket_notes`（migration 009，append-only：`id`=`tn_<16×base32>`、`ticket_id`、`author`、`body ≤8KiB`、`at`）。`author` = principal（未鉴权记 `"anonymous"`，对齐 §13.2 审计口径）。GET `/v1/tickets/{id}` 内联 `notes[]`（按 `at` 升序）。备注**仅追加，不可改删**。
- **指派（assignee，PRMT-060，as-built）**：`POST /v1/tickets/{id}:assign` body `{assignee}` 更新既有 `assignee` 字段（空串=取消指派），不改其它字段。
- **状态转换审计（PRMT-061，as-built）**：`ticket_audit`（migration 010，append-only，`op ∈ {created,transitioned,assigned}` CHECK）：create / `:transition` / `:assign` 成功后留痕（`from_state`/`to_state`/`who`/`at`）。GET `/v1/tickets/{id}:history`（list-scope）。**best-effort**：审计写失败不回滚 ticket（对齐 §13.2 / Q12）。
- **去重唯一索引（PRMT-081，as-built，Q15）**：`migration 011` 建**部分唯一索引** `tickets(alarm_id) WHERE alarm_id<>'' AND state<>'closed'`——把 §4 的"一活跃来源键 ↔ 至多一未关闭 ticket"由 PG 强制（先前仅扫描器查后插，并发可重复）。插入冲突 → 哨兵 `ErrDuplicateActiveTicket`，调用方退化为 no-op（幂等）。手动单（`alarm_id=''`）不受约束。`<source>:<id>` 命名空间见 §18。
- **乐观版本 CAS（PRMT-082，Q16；as-built，commit 09a6967）**：`tickets.resource_version BIGINT`（migration 012，每次成功写 +1）。`Store.PutTicket(ctx, t, expectVersion)`：`expectVersion>0` 时 CAS，不符返回 `ErrVersionConflict` + 当前 ticket。`:transition`/`:assign` handler 读当前版本→CAS→冲突映射 **409**（RFC 7807）；`create` 用 `expectVersion=0`（强制）。目的：消除并发转换的丢更新（取代 last-writer-wins）。**实现状态**：as-built（commit `09a6967`）——store CAS + migration 012 + transition/assign handler CAS→409 全落地。

---

# 2. Ticket 状态机（Q1 → 建议含 acknowledged）

```
合法转换：
  open          → acknowledged | closed
  acknowledged  → resolved     | closed
  resolved      → closed
  （任意非 closed 态 → closed：管理性关闭/取消）
非法转换 → HTTP 422（RFC 7807 type=invalid-transition）；不可回退。

转换写时间戳：
  → acknowledged : acked_at    = now
  → resolved     : resolved_at = now
  → closed       : closed_at   = now
```

**决策 Q1（建议：含 acknowledged 四态）**——保留 `acknowledged` 而非 open→resolved 直转。理由：(1) §3 SLA 需"响应时限"=`acked_at − opened_at`，无 ack 无法度量响应；(2) 与 spec-003 告警 `acked` 态语义对齐；(3) MTTR 度量更细。代价：多一步操作。**待 Yuri 评审。**

---

# 3. SLA（Q3 → 建议默认值；实现于 M2 P504）

每 ticket 按 severity 套用响应/解决时限；超时产生升级事件（§5）。**SLA 字段与计时器是 P504 内容，v0.1 仅定口径与默认值。**

| severity | 响应时限（→acknowledged）| 解决时限（→resolved）|
|----------|------------------------|---------------------|
| critical | 15 min | 4 h |
| major | 1 h | 24 h |
| minor | 8 h | 72 h |
| info | 无 SLA（best-effort）| 无 SLA |

- 时限默认值**可配置**（站点/租户级覆盖）；上表为 v0.1 建议默认。
- 度量口径：**响应时间 = `acked_at − opened_at`；MTTR（恢复）= `resolved_at − opened_at`**；`closed_at` 为管理性关闭（不计入 MTTR）。MTBF = 同一 `asset_path`（或 `alarm` 规则）相邻两次 open 的平均间隔。
- 超时 = `now − opened_at > 时限` 且对应时间戳未达 → 触发 §5 escalation 事件。

**决策 Q3（建议上表默认）。待 Yuri 评审具体数值。**

---

# 4. Alarm → Ticket 自动开单（Q2 → 建议按 alarm_id 去重）

cios-alarm 在告警 `firing` 转换时（spec-003 §4），若启用 `-auto-ticket`，自动开一张 ticket（PRMT-034）。

- 映射：`alarm_id`=该告警 `alarms.id`；`asset_path`=告警 AssetPath；`title`=告警 summary（空则 rule name）；`severity`=告警 severity；`state`=open；`opened_at`=now。
- **去重（Q2，建议按 alarm_id）**：开单前查 `SELECT 1 FROM tickets WHERE alarm_id=$1 AND state <> 'closed'`，命中则跳过——**一个活跃告警实例 ↔ 至多一张未关闭 ticket**。理由：`alarm_id`（=rule+path 实例键）是稳定的"一次告警发作"键；`asset+rule` 更粗，会把不同发作期合并。代价：同一资产并发多规则各开一单（符合预期）。
- fail-soft：开单失败仅 log，不阻断告警写入/CloudEvents（spec-003 链路优先）。
- 闭环（§M2-1）：firing→自动 open→operator `ack/resolve/close`（§2）。**自动关单不在 v0.1**（告警 resolved 不自动关 ticket；人工确认关闭，避免误闭）。

**决策 Q2（建议 alarm_id 去重）。待 Yuri 评审。**

---

# 5. Ticket 生命周期事件（复用 spec-003 CloudEvents profile）

每次 ticket 创建/转换发布 CloudEvents 1.0（spec-003 §1）：

| type | 含义 |
|------|------|
| `io.cios.ticket.opened` | 新开（手动或自动）|
| `io.cios.ticket.transitioned` | 状态转换（含 to-state）|
| `io.cios.ticket.escalated` | SLA 超时升级（§3，P504）|

- `id`=UUIDv7；`source`=`cios://<site>/cios-core`（或 cios-alarm 自动开单时其身份）；`subject`=`asset_path`；扩展属性 `severity`、`site`。
- 出口 webhook（Jira/飞书，E2.3 P505）消费该事件流。**v0.1 仅定 type 注册；webhook 实现是 P505。**
- **Email 渠道（P783 / L105，additive）**：同一事件流可 fan-out 到 SMTP（`cios-core` `-ticket-smtp-*` / `CIOS_TICKET_SMTP_*`）。固定收件人列表（订阅模型后置）；plain-text 模板；fail-soft（发送失败只记 log，不阻断 ticket 写路径）。**不**扩 Slack/飞书/Teams/SMS（L105）。

---

# 6. 关联模型（全链可追溯）

```
ticket.alarm_id   → alarms.id        (firing 告警，spec-003)
ticket.asset_path → assets.path      (资产，spec-001)
ticket ← spare.consumed_by           (备件消耗，E2.5 P541，反向)
alarmrule.runbook → ticket 渲染       (spec-003 §3 runbook 字段，E2.8 P571)
```
- ticket↔alarm↔asset 在 v0.1 经字段直连；spare/runbook 链在各自 epic 接入。

---

# 7. API 面（细节遵 spec-004）

| 方法 | 路径 | 角色门槛 | 说明 |
|------|------|---------|------|
| GET | `/v1/tickets` | viewer | 分页 + per-item scope filter（on `asset_path`，复用 alarms）|
| POST | `/v1/tickets` | operator | 手动开单（state=open）|
| GET | `/v1/tickets/{id}` | viewer | 单条；404 RFC 7807 |
| POST | `/v1/tickets/{id}:transition` | operator | body `{to}`；§2 状态机；非法 422 |

- RBAC：read=viewer / create+transition=operator(control:write) / admin 旁路（含未来 delete）。L50 scope：读隐含子树、写显式（同 alarms）。
- `Auth==nil` → M0 行为放行（向后兼容）。
- 错误一律 RFC 7807（spec-004）。
- CLI 镜像：`cios ticket list/get/open/ack/resolve/close`（PRMT-033）。

---

# 8. ID 方案（Q4 → 已决 L69：`tk_`+base32）

**决策 Q4（已决 L69，2026-06-19）**——ticket `id` = `"tk_"` + 16 个大写 base32 字符（RFC 4648 无填充，[A-Z2-7]），由 10 随机字节生成（~80 bit；正则 `^tk_[A-Z2-7]{16}$`，与 `core.newTicketID` 一致）。理由：(1) 多站/fleet 全局唯一，无需中心计数器；(2) `tk_` 前缀在日志/URL 中易辨识，且与其他标识符区分；(3) **列表/分页按 `opened_at` 排序（§1.2 / §7），不依赖 ID 时序**，故 UUIDv7 的时间有序优势在此无用。**说明**：本条 2026-06-19 由初版「UUIDv7」调整为 `tk_` 方案，与已交付且 tested 的 PRMT-033 `newTicketID` 对齐（code-wins-once-tested，L69 同步更新）。备选 `site-seq`（需 per-site 计数器，fleet 协调/碰撞风险，弃）；如需人类友好引用，可后续加只读 `display_ref`（不作主键）。

---

# 9. Preventive Maintenance（E2.4）—— v0.3 as-built（PRMT-043）

PM = 周期性维护计划，到期自动开 ticket（与告警自动开单 §4 同一 ticket 模型）。持久化 `pm_schedules`（migration 004）。

## 9.1 PMSchedule 字段

| 字段 | 类型 | 必填 | 语义 |
|------|------|------|------|
| `id` | string `pm_<base32>` | ✓ | 全局唯一（同 `tk_` 形态，§8 同理由）|
| `asset_path` | string (crn) | ✓ | 目标资产；RBAC scope 维度 |
| `title` | string | ✓ | 维护项摘要（开单时进 ticket.title）|
| `severity` | enum | ✓ | 复用 spec-003（开单时进 ticket.severity）|
| `interval` | duration | ✓ | 日历周期（如 `720h`）；`next_due += interval` |
| `next_due` | timestamptz | ✓ | 下次到期；扫描器据此触发 |
| `enabled` | bool | ✓ | false=跳过 |

## 9.2 触发语义（Q9）

- **calendar-only（v0.3）**：扫描器 `RunPMScanner`（ctx 控制后台 goroutine，复用 §3 SLA 扫描器模式：interval guard + startup scan + ticker + ctx.Done），周期由 `-pm-scan-interval`（默认 1h）。
- 到期判定 `next_due <= now && enabled` → 开 ticket（`alarm_id` 空，区别于告警单）。
- **幂等（Q10）**：扫描器单实例 + ticker 非重叠，内存中 `next_due` 是下一 tick 的幂等闸门（下一 tick 见 `next_due>now` 即跳过）。开单顺序 = **fire-then-advance**（先 `PutTicket` 成功、**再** `next_due += interval`）；**开单失败则不推进**（避免某资产 ticket 路径持续故障时永久静默停摆——宁可下一 tick 重试，最坏一张重复单，可人工关闭）。PM（§9）扫描器与巡检扫描器（§17，PRMT-049）为同一实现。多实例 leader 选举（T43）由 PRMT-065 的 `TryScannerLock`（pgStore advisory lock / fileStore no-op）收口——6 扫描器 tick 入口统一加锁，详见 §18。
- `enabled=false` / 未到期 → 跳过。
- **计量触发（runhours-based）= 未实现**（Q9 余项）：M3 接 VM 运行时累计量后启用；当前仅日历。记 TODO **T41**。
- **维护窗口 ↔ 告警抑制**（P532）= 不在本 epic（spec-003 告警侧），记 TODO **T42**。
- **leader election**：`RunPMScanner` 不防多实例并发，单实例假设；fleet 多实例 = M3，记 TODO **T43**（与 §3 SLA / §11 报表调度共解）。

## 9.3 API（细节遵 spec-004）

| 方法 | 路径 | 角色门槛 | 说明 |
|------|------|---------|------|
| GET | `/v1/pm/schedules` | viewer | 分页 + per-item scope filter（on `asset_path`）|
| POST | `/v1/pm/schedules` | operator (control:write) | 创建 |
| GET | `/v1/pm/schedules/{id}` | viewer | 单条；404 RFC 7807 |

- **authmw 必注册**（防 PRMT-037 类 RBAC 旁路）：list-scope GET（角色底 + per-item authorize）/ POST = ActionControlWrite。

---

# 10. Capacity Management（E2.2）—— v0.3 as-built（PRMT-040）

实时余量 = 额定（CMDB）− 实测 P95（VM）。满足 M2 退出 §M2-3（容量决策）。

## 10.1 计算口径（Q8）

- **rated**：`Asset.Spec["rated_power_w"]`（接受 float64/int/int64/json.Number/纯数字串 `"1500"`；带单位后缀如 `"1500W"` 按缺失计 → `missing_rated` 计数器 +1）。
- **measured_p95**：VM instant query `quantile_over_time(0.95, cios_power_watt{asset_path="<path>"}[<window>])`，复用 `core/metrics.go` `fetchVM`（5s 超时）。
- **remaining = rated − measured_p95**。
- **fail-soft**：任一资产 VM 查询失败/空向量 → 该资产 `measured_p95/remaining=null` + `degraded=true`（不 500 整请求）。

## 10.2 维度分期（Q7 → v0.4 扩 cooling/rack）

- **power = v0.3 实现**（rated `Spec.rated_power_w`）。
- **cooling = v0.4 实现（PRMT-055，Q17）**：`remaining_kw = Spec.rated_cooling_kw − P95(quantile_over_time 0.95 cios_heat_kw[window])`；**仅 host∈{cdu,chiller} 参与**（其余在 cooling 维跳过，不计 missing）；fail-soft `degraded` 同 power。
- **rack = v0.4 实现（PRMT-062，Q17）**：`remaining_w = Spec.rated_rack_power_w − Σ(在架子设备 P95 `cios_power_watt`)`，子设备 = path 前缀 `<rackPath>.` 枚举；**仅 host=rack 参与**；fail-soft 同上。
- **gpu = `not_implemented` 占位**（同 envelope 形，`status:"not_implemented"`）；实化待 GPU 遥测期（L68）。
- **新增 Spec 额定键**：`rated_cooling_kw`（kW，cdu/chiller）、`rated_rack_power_w`（W，rack）——v0.4 引入的运行期约定（additive，不破 spec-001 v1.0 冻结；建议 spec-001 附录 v1.1 登记）。类型接受同 `rated_power_w`。
- **N+1 what-if / 制冷冗余 headroom**（P524 / T38）= 待冗余拓扑模型，仍 placeholder。

## 10.3 过滤 + API + 导出

- **lifecycle 过滤**：仅 `active` / `maintenance` 计入（planned/installed/retired/缺失排除）。
- GET `/v1/capacity?window=7d&filter=G` → list-scope（authmw 注册，与 `/v1/alarms` 同形：角色底 + per-item `authorize(ActionRead, asset.Path)`，out-of-scope 静默丢弃）。POST → 405。bad window/filter → 400。
- **Forecast（P741，additive）**：GET `/v1/capacity/forecast?horizons=30d,90d&growth_pct_per_year=0&window=7d&filter=G` → 对 power/cooling 用 capacity 基线 measured P95 做 **linear_growth** 推算（`forecast_measured = measured × (1+g)^(days/365)`）；默认 g=0（持平）。**gpu 维 = `not_implemented`** 直至 P761 DCGM。list-scope 同 `/v1/capacity`。advisory only。
- **Prometheus 导出（PRMT-056，Q18）**：GET `/v1/capacity/metrics` → text exposition（`cios_capacity_rated_watt` / `cios_capacity_remaining_watt` / cooling 同形 `_kw`；degraded 资产输出 `cios_capacity_degraded{asset_path} 1` 并跳 remaining 行），label 值经 `escapeLabelValue` 转义。**authmw 注册 list-scope**（抓取方须带 viewer token，不裸露）。供 P522 容量页抓取。
- **PromQL 注入防御（PRMT-078）**：所有把 `asset_path` 拼入 label matcher 的 sink（capacity 查询 + §11 对账查询）用 `escapeLabelValue` 转义（纵深防御，叠加 cpath 字符集校验）。
- CLI：`cios capacity [--window 7d] [--filter G]`（json/yaml/table 三模式）。

---

# 11. Reporting（E2.6）—— v0.3 as-built（PRMT-037 + PRMT-042）

运维报表 = MTTR/MTBF + 告警 Top + 计数，满足 M2 退出 §M2-2（MTTR/MTBF）/ §M2-4（月报自动）。

- **统计口径**：`computeOpsReport`（PRMT-037）—— MTTR=`resolved_at−opened_at` 均值、响应=`acked_at−opened_at` 均值、MTBF（告警间隔）、`topAlarmsByPath`（按 path 计数 Top）。与 §3 Q3 口径一致。
- **API**：GET `/v1/reports/ops`（viewer；**authmw 注册** list-scope，PRMT-038 修复 037 的 RBAC 旁路后冻结）。
- **离线渲染**：`renderOpsHTML` → 自包含 HTML（MTTR/MTBF/计数/Top + 可用性占位）。
- **定时**：`RunReportScheduler`（§3 SLA 扫描器模式：startup+ticker+ctx.Done、fail-soft），`-report-dir`（空=关）/ `-report-interval`（默认 24h）。
- **CLI**：`cios report generate`（本地 HTML 渲染）。
- **scope（Q11）**：调度报表为**站点全景**（无 principal/scope filter）——站点运营报表语义。多租户/scoped 报表 = M3（与 T37 客户状态页同期），记 TODO **T45**。
- **账实对账（PRMT-050）**：GET `/v1/reports/reconcile?window=7d` = CMDB 注册资产 vs VM 近窗实采的差异（`registered_no_telemetry` / `telemetry_no_asset` orphan / `ok`），lifecycle 过滤 active|maintenance、per-item scope、VM 不可达 fail-soft `degraded`。**orphan 列表（无 asset 可做 path-scope）按角色底 operator+ 管控**：viewer 收到空 `orphans` + `orphans_restricted=true`（防拓扑泄漏）。authmw 注册（list-scope，同 `/v1/reports/ops`）。`cios report reconcile`。
- **对账漂移自动开单（PRMT-057，Q14，opt-in）**：`RunReconcileScanner`（`-reconcile-scan-interval`，**默认 0 = 不启动**）周期跑 `computeReconcile`，对持续 `registered_no_telemetry` 资产开 major ticket；**去重 `alarm_id="reconcile:<path>"`**（§18 命名空间）；`degraded`（VM 不可达）→ 整 tick 跳过开单（不误开）；遥测恢复不自动关（Q5）。
- **报表保留 + 索引（PRMT-064）**：`-report-keep`（默认 30，0=不限）每次生成后按命名前缀**仅删自产报表**（白名单式，绝不 RemoveAll）；重写 `index.html`（`html/template` 转义，按时刻倒序列出）；保留/索引失败 fail-soft。

---

# 12. Runbook / Cases（E2.8）—— v0.3 as-built（PRMT-044）

- **Ticket.Runbook**（migration 005，`TEXT NOT NULL DEFAULT ''`）：工单挂 runbook key。
- **Runbook 内容只读**：GET `/v1/runbooks/{key}`（文件读，**三重路径遍历防护**：正则白名单 + `path.Clean` + abs-dir 包含校验；`.md`），`-runbook-dir` 配置根。写 API = M3+。
- **Cases（案例库）**：GET `/v1/cases` = 已关闭 ticket 集（M4 AI 语料来源）。
- **案例库检索 + 导出（PRMT-053，v0.4）**：`/v1/cases` 加查询参数 `severity` / `asset_prefix` / `since` / `until`（按 `closed_at`）/ `limit` / `format=csv`。**过滤顺序：取 closed → 字段过滤 → per-item scope 丢弃 → limit**（scope 不被绕过）。CSV 用标准库 `encoding/csv`，列序 `id,severity,asset_path,title,opened_at,closed_at,runbook`。全文/AI 检索仍 = M4。
- **authmw 必注册**：`/v1/runbooks/`、`/v1/cases` list-scope GET。
- **CLI**：`cios case list [--severity --asset-prefix --since --until --limit --csv]`。
- **来源链（Q13，PRMT-047 as-built）✅**：告警引擎 `instance.tick()` 三处 `Event` 构造点已从 `rule.Spec.Annotations["runbook"]` 填 `Event.Runbook`（注解缺失=`""`，向后兼容）→ 经 `store.OpenTicket(ev.Runbook)` 贯穿到 ticket。来源链闭合。

---

# 13. CMDB Operations：生命周期 + 变更审计（E2.1）—— v0.3 as-built（PRMT-039 + PRMT-045）

## 13.1 资产生命周期（Q6，PRMT-039）

- **状态集**：`planned → installed → active → maintenance → retired`（存于 `Asset.Spec["lifecycle"]`，**无 spec-001 schema 变更**，spec-001 v1.0 冻结线不破）。
- 缺省 = `planned`（PUT 无 `lifecycle` 字段时）。
- **合法转换**：`allowedLifecycleTransition(from,to)` 表约束；非法 → 拒。
- POST `/v1/assets/{path}:lifecycle` 单资源写（**全鉴权**，非 list-scope，与 `:set` 同形；authmw 注册）。
- **retired → 停采集**（gateway↔CMDB 联动）= 子项另案，记 TODO（已在 E2.1 范围，T 内）。

## 13.2 变更审计（Q12，PRMT-045）

- **append-only** `asset_audit`（migration 006，`op` CHECK 约束）：PUT / lifecycle / DELETE 成功后留痕。
- `who` = principal（未鉴权 `hasAuth==false` 时记 `"anonymous"`，§契约）。
- GET `/v1/assets/{path}:history`（list-scope GET，authmw 注册）。
- **best-effort**：审计写失败**不回滚** asset（§Q12 决议）；强一致前置事务 = M3。保留期 / 防篡改签名 = M3（C1 = T40）。

## 13.3 资产检索 + 批量（v0.4，PRMT-067/068）

- **检索过滤（PRMT-067）**：GET `/v1/assets?type=&lifecycle=&prefix=&limit=`（叠加 AND；`lifecycle` 非法值 400）。**过滤顺序：取全量 → 字段过滤 → per-item scope → limit**（scope 不被绕过）。路径/角色/authmw 不变（仅加 query）。
- **批量 export/import（PRMT-068，CLI-only）**：`cios asset export [--prefix --format csv|yaml]`（按 path 排序，稳定可 diff）；`cios asset import -f <file> [--dry-run]`（逐条走既有 PUT，幂等 upsert；嵌套 `Spec` 以 `spec.<key>` 扁平化列还原；单条失败继续+汇总+非零退出）。**无新服务端端点**（客户端循环）。

---

# 16. Spare Part（E2.5）—— v0.4 as-built（PRMT-048/054）

最小备件域：目录 + 库存水位 + 出入库流水 + 与 ticket 的消耗关联；**不做采购/供应商/价格**。持久化 `spare_parts` + `spare_txns`（migration 007）。

## 16.1 模型

| 表 | 字段 | 语义 |
|----|------|------|
| `spare_parts` | `id`=`sp_<16×base32>`、`sku`(唯一)、`name`、`qty`(≥0)、`min_qty`、`location?` | 目录 + 当前库存；`low_stock` = `qty<min_qty`（派生，不持久化）|
| `spare_txns` | `id`=`st_<16×base32>`、`spare_id`(FK)、`delta`(≠0)、`ticket_id?`、`at` | append-only 出入库流水；出库领料时 `ticket_id`=`tk_…` |

## 16.2 语义 + API

- **库存唯一变更入口 = `:adjust`**（无直接 PUT qty）：`POST /v1/spares/{id}:adjust` body `{delta, ticket_id?}` → 写一条 txn + 更 `qty`，**原子**（pgStore 单事务 `SELECT…FOR UPDATE`；fileStore 单 mutex 全程）；`qty+delta<0` → `ErrInsufficientStock` → 422；`delta=0` → 400；`ticket_id` 非空须 `tk_` 前缀。
- **SKU 唯一**：create 时异 id 同 sku → `ErrSKUExists`（pgStore `UNIQUE` 约束 / fileStore 持锁校验，**双实现一致**，PRMT-080）。
- **scope**：备件无 `asset_path` → `/v1/spares` 用**角色底 list**（viewer 读 / operator 写），不做 per-item path scope（区别于 alarms/tickets）。authmw 注册。
- **API**：GET `/v1/spares`（viewer）/ POST（operator）/ GET `/v1/spares/{id}`（viewer，含 `low_stock` + 最近 txn）/ POST `:adjust`（operator）。CLI `cios spare list/get/create/adjust`。
- **低水位自动开单（PRMT-054）**：`RunSpareStockScanner`（`-spare-scan-interval` 默认 1h）对 `qty<min_qty` 开 minor ticket；**去重 `alarm_id="spare:<id>"`**（§18）；回补后不自动关（Q5）。

---

# 17. Inspection（E2.7）—— v0.4 as-built（PRMT-049/059/063）

巡检模板按周期自动开巡检单（与 PM §9 同构）+ 移动端响应式勾选/拍照。持久化 `inspection_templates`（migration 008）。

## 17.1 模型 + 扫描器

| 字段 | 语义 |
|------|------|
| `id`=`ins_<16×base32>`、`asset_path`、`title`、`items`(json []string)、`interval`、`next_due`、`enabled` | 巡检模板 |

- **扫描器 `RunInspectionScanner`（`-inspection-scan-interval` 默认 1h）**：镜像 PM（§9.2）——到期开 ticket（`severity=info`、`alarm_id=""`、`items` 序列化进既有 `Runbook` 字段，**不改 ticket schema**）、fire-then-advance 幂等、leader lock（§18）。
- **API**：GET `/v1/inspections`（viewer，list-scope per-item on asset_path）/ POST（operator）/ GET `/v1/inspections/{id}`（viewer）。authmw 注册。CLI `cios inspection list/get/create`。

## 17.2 移动端 Web（PRMT-059/063）

- **勾选表（PRMT-059）**：GET `/v1/inspections/form/{ticketID}` → `text/html` 服务端渲染（**`html/template` 自动转义防 XSS**）；items 从 ticket.Runbook（`inspection:` 前缀）解析；非巡检 ticket → 404。POST 同路径 → 经 `allowedTransition` 走 `resolved`（非法起态 422），结果写回既有字段。**单资源授权**（按 ticket.asset_path，GET viewer / POST control:write）。
- **照片上传（PRMT-063）**：POST `/v1/inspections/form/{ticketID}/photo`（multipart）→ `-inspection-photo-dir/<ticketID>/<safeName>`；**`MaxBytesReader` 先于 parse** + 扩展名白名单（jpg/jpeg/png/pdf，否则 415）+ MIME 嗅探 + runbook 同款三重路径遍历防护；dir 空 → 503 disabled。

---

# 18. alarm_id 来源命名空间约定（v0.4，PRMT-054/057/081，Q19）

`tickets.alarm_id` 同时承载「告警 id」与「派生工单来源 key」两种语义，统一为 **`<source>:<id>` 命名空间**（互斥、`:` 前缀可辨识）：

| source | 形如 | 产生者 | 去重语义 |
|--------|------|--------|----------|
| （无前缀）| `alarms.id`（rule+path 实例键）| cios-alarm 自动开单（§4）| 一活跃告警实例 ↔ ≤1 未关闭 ticket |
| `spare:` | `spare:<spareID>` | 低水位扫描器（§16.2）| 一低水位备件 ↔ ≤1 未关闭 ticket |
| `reconcile:` | `reconcile:<assetPath>` | 对账漂移扫描器（§11）| 一漂移资产 ↔ ≤1 未关闭 ticket |
| （空 `''`）| 手动工单 | `POST /v1/tickets` | 不去重（部分唯一索引 WHERE 排除空）|

- §1.3 的部分唯一索引 `tickets(alarm_id) WHERE alarm_id<>'' AND state<>'closed'` 对**全部** source 统一生效。
- **扫描器 leader 选举（PRMT-065，T43）**：6 个后台扫描器（sla/pm/inspection/spare/reconcile/report）tick 入口经 `Store.TryScannerLock(name)`——pgStore `pg_try_advisory_lock`（per-tick 取/释放，非常驻会话锁）/ fileStore no-op（单实例恒 leader）。多实例下同一扫描器一轮只一个实例执行。

---

# 19. 未决问题（评审拍板 → 移入 LOCKED + 升版）

| # | 问题 | 架构者建议 | 状态 |
|---|------|-----------|------|
| Q1 | Ticket 状态机是否含 `acknowledged` | **含**（四态 open→ack→resolved→closed；SLA 响应时限 + spec-003 对齐需要）| ✅ 已决 L69 (2026-06-19) |
| Q2 | 自动开单去重粒度 | **alarm_id**（一活跃告警实例 ↔ 至多一未关闭 ticket）| ✅ 已决 L69 (2026-06-19) |
| Q3 | SLA 默认时限（按 severity）| §3 默认表（critical 15m/4h … info 无 SLA）；可配置 | ✅ 已决 L69 (2026-06-19) |
| Q4 | ticket ID 方案 | **`tk_`+16×base32（80-bit 随机）**（多站全局唯一 + `tk_` 前缀辨识；列表按 opened_at 排序故不需 ID 时序）；可选只读 `display_ref` | ✅ 已决 L69 (2026-06-19) |
| Q5 | 告警 resolved 是否自动关 ticket | **否**（人工确认关闭，避免误闭）；v0.1 仅自动开 | ✅ 已决 L69 (2026-06-19) |
| Q6 | 资产生命周期状态集 | `planned→installed→active→maintenance→retired`（存 `Spec["lifecycle"]`，不破 spec-001 冻结）| ✅ 已决 L70 (2026-06-20) |
| Q7 | Capacity 维度分期 | M2 先 `power`；`cooling/rack/gpu` = `not_implemented` 占位（同 envelope 形）| ✅ 已决 L70 (2026-06-20) |
| Q8 | Capacity 余量口径 | `rated(Spec.rated_power_w) − P95(quantile_over_time 0.95 cios_power_watt[window])`；fail-soft degraded | ✅ 已决 L70 (2026-06-20) |
| Q9 | PM 触发方式 | v0.3 **calendar-only**；计量(runhours)触发 = M3（T41）| ✅ 已决 L70 (2026-06-20) |
| Q10 | PM 幂等 | 单实例 + 非重叠 ticker；**fire-then-advance**（开单成功后才推进 `next_due`，失败不推进 → 重试而非静默停摆）| ✅ 已决 L70 (2026-06-20) |
| Q11 | 报表 scope | 调度报表 = **站点全景**（无 scope）；多租户/scoped = M3（T45）| ✅ 已决 L70 (2026-06-20) |
| Q12 | 资产审计一致性 | **append-only + best-effort**（写失败不回滚 asset）；强一致/保留期/防篡改 = M3 | ✅ 已决 L70 (2026-06-20) |
| Q13 | runbook 来源链 | 引擎从 `rule.Spec.Annotations["runbook"]` 填 `Event.Runbook` → ticket（缺失=`""`）| ✅ 已决 L71 (2026-06-21，PRMT-047) |
| Q14 | 对账漂移自动开单 | **opt-in**（`-reconcile-scan-interval` 默认 0=关）；degraded 跳过；dedup `reconcile:<path>`；不自动关 | ✅ 已决 L71 (2026-06-21，PRMT-057) |
| Q15 | ticket 去重落地 | schema 层**部分唯一索引** `tickets(alarm_id) WHERE alarm_id<>'' AND state<>closed`（兜底，叠加扫描器查后插）| ✅ 已决 L71 (2026-06-21，PRMT-081) |
| Q16 | ticket 转换并发 | **乐观版本 CAS**（`resource_version` + `expectVersion` + `ErrVersionConflict`→409）取代 last-writer-wins | ✅ 已决 L71（契约）；handler 接线随硬化批 |
| Q17 | Capacity cooling/rack 口径 | cooling=`rated_cooling_kw−P95(cios_heat_kw)`@cdu/chiller；rack=`rated_rack_power_w−Σ子设备 P95`@rack；gpu 仍占位 | ✅ 已决 L71 (2026-06-21，PRMT-055/062) |
| Q18 | Capacity 导出 | `/v1/capacity/metrics` Prometheus text，list-scope 鉴权，label 转义；供 P522 抓取 | ✅ 已决 L71 (2026-06-21，PRMT-056) |
| Q19 | alarm_id 命名空间 | `<source>:<id>`（无前缀=告警 / `spare:` / `reconcile:` / 空=手动）；扫描器 leader 经 `TryScannerLock` | ✅ 已决 L71 (2026-06-21，§18) |

> Q1–Q5 = Ticket（L69）。Q6–Q12 = PM/Capacity/Reporting/Runbook/CMDB-Ops as-built（L70）。**Q13–Q19 = Spare/Inspection/容量多维·导出/对账扫描/工单备注·审计·版本·去重 + 命名空间的 as-built 追认（L71，2026-06-21，code-wins-once-tested：PRMT-047/048/049/050/053/054/055/056/057/059/060/061/062/063/064/065/067/068/081 已 tested；PRMT-082 契约已决、handler 接线随 075–084 硬化批）**。剩余余项（计量 PM T41 / 维护窗口抑制 T42 / capacity N+1 T38 / gpu 维 / scoped 报表 T45 / 碳-CUE T30）见各 § 内嵌 TODO。

---

# 20. CHANGELOG

| 版本 | 日期 | 变更 |
|------|------|------|
| v0.1 | 2026-06-19 | 草稿创建：Ticket 完整规范（§1–§8，M2 E2.3 脊柱权威源）+ PM/Capacity v0.2 占位；Q1–Q5 待评审；与 PRMT-032/033/034 契约对齐 |
| v0.2 | 2026-06-19 | **Q1–Q5 批准（L69，Yuri follow architect rec）→ Ticket 段（§1–§8）冻结**；可签发 PRMT-032/033/034 编码。PM（§9）/Capacity（§10）仍占位，待 P53x/P52x 排期升 v0.3 |
| v0.2 (amend) | 2026-06-19 | **Q4 调整：ticket ID UUIDv7 → `tk_`+16×base32**，对齐已交付且 tested 的 PRMT-033 `newTicketID`（code-wins-once-tested，Yuri 同意；L69 同步）。§1.1/§8/§11 已改；CloudEvents envelope `id`（§5）仍 UUIDv7（spec-003 §1.1，不变）|
| **v0.3** | **2026-06-20** | **PM（§9）/ Capacity（§10）/ Reporting（§11）/ Runbook-Cases（§12）/ CMDB-Ops 生命周期+审计（§13）由占位升 as-built 契约**，对齐已 tested 的 PRMT-039/040/042/043/044/045（code-wins-once-tested）。Q6–Q12 批准（**L70**，Yuri 2026-06-20）。旧 §11/§12（Q-table/CHANGELOG）顺延为 §14/§15。余项（计量 PM、维护窗口抑制、scanner leader election、capacity 多维、scoped 报表、runbook 来源链 PRMT-047）记 T41–T45。Ticket 段（§1–§8）不动。|
| **v0.4** | **2026-06-21** | **新增 §16 Spare（PRMT-048/054/080）/ §17 Inspection（PRMT-049/059/063）/ §18 alarm_id `<source>:<id>` 命名空间 + 扫描器 leader（PRMT-054/057/065/081）**；扩 §10 Capacity cooling·rack·`/v1/capacity/metrics` 导出·PromQL 转义（PRMT-055/056/062/078）、§11 对账扫描·报表保留（PRMT-050/057/064）、§12 来源链闭合·案例检索（PRMT-047/053）、§13 资产检索·批量（PRMT-067/068）、§1.3 工单备注·指派·转换审计·去重唯一索引·乐观版本（PRMT-060/061/081/082）。Q13–Q19 批准（**L71**）。新增 Spec 额定键 `rated_cooling_kw`/`rated_rack_power_w`（additive）。旧 §14/§15 顺延为 §19/§20。**PRMT-082 handler CAS 接线随 075–084 硬化批落地**（其余均 tested+archived）。Ticket 脊柱段（§1.1–§8）不动。|
