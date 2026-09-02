# CIOS Spec 003 — Events & Alarms

> 版本 **v1.0（冻结，2026-06-13 评审会）**｜ 依赖：spec-001 §2（路径）、spec-002 §5（状态枚举）、spec-006 §2.2（NATS subject）
> 分期：M0 实现 firing/acked/resolved 三态子集（无规则引擎）；**suppressed 与规则引擎 M1 E1.6 启用**（L56）
> 覆盖 TODO T8：CloudEvents profile、severity 分级、告警规则 YAML、告警状态机。

---

# 1. 事件模型（CloudEvents 1.0 Profile）

所有事件（告警、生命周期、审计、控制结果）统一为 CloudEvents 1.0 JSON 编码，
经 NATS 发布到 spec-006 §2.2 定义的 subject。

## 1.1 必填属性约定

| CE 属性 | CIOS 约定 |
|---------|----------|
| `id` | UUIDv7（时间有序，便于去重与排序） |
| `source` | 产生组件身份：`cios://<site>/<component>`（同 spec-006 §5.1，如 `cios://site01/cios-alarm`） |
| `type` | 命名空间 `io.cios.<类别>.<子类>`，见 §1.2 |
| `subject` | 关联的**点位地址或资产路径**（spec-001 §2 / spec-002 §1） |
| `time` | RFC3339，事件发生时刻（非发布时刻） |
| `datacontenttype` | `application/json` |

扩展属性（CIOS 自定义，全小写）：`severity`（§2，仅告警类）、`site`（冗余站点代号，
便于云端按属性过滤）。

## 1.2 type 注册表

| type | 含义 | NATS subject |
|------|------|--------------|
| `io.cios.alarm.firing` | 告警触发 | `cios.evt.<site>.alarm` |
| `io.cios.alarm.resolved` | 告警恢复 | `cios.evt.<site>.alarm` |
| `io.cios.alarm.ack` | 告警确认（人工） | `cios.evt.<site>.alarm` |
| `io.cios.lifecycle.changed` | 资产生命周期变更（spec-001 lifecycle） | `cios.evt.<site>.lifecycle` |
| `io.cios.occupancy.changed` | 换件/移位 occupancy 记录（spec-001 §6） | `cios.evt.<site>.lifecycle` |
| `io.cios.audit.action` | 审计（who/when/what/from-where） | `cios.evt.<site>.audit` |
| `io.cios.cmd.result` | Set 执行结果与回读（spec-002 §8） | `cios.cmdres.<site>` |

新增 type = 修订本表（PR 评审）。消费者必须容忍未知 type（向前兼容）。

---

# 2. Severity 分级

| 级别 | 语义 | 响应预期 | 示例 |
|------|------|---------|------|
| `critical` | 危及设备/数据/人身，需立即处置 | 立即（24×7 呼叫） | **漏液（leak=1，T16 首批）**、火警、断路器意外分闸、UPS 掉电 |
| `major` | 功能受损或冗余丢失，需尽快处置 | <30min | 冗余泵失效、CDU fault、GPU 温度越限 |
| `minor` | 性能劣化/带病运行 | 工作时间内 | ΔT 偏低、风扇转速异常、单点 stale |
| `info` | 提示性，无需处置 | 无 | 维护开始/结束、配置变更生效 |

- severity 由**告警规则**声明，不由设备私有码决定（私有码经点表 enum_map 归一后参与规则）
- maintenance 状态（spec-002 §5 值 4）期间，该资产路径子树的告警**抑制**（§4 suppressed）

---

# 3. 告警规则（AlarmRule YAML）

告警规则包是型号包的组成部分（spec-001 §4.5），与点表同级分发、签名（spec-006 §5.5）。

```yaml
kind: AlarmRule
metadata:
  name: cdu-fws-deltat-low          # 全局唯一, kebab-case
  appliesTo: cdu                    # 资产类型 (types.yaml); 实例化时按类型展开
spec:
  expr: "fws.deltat < 4"            # 相对点位表达式; 比较运算 + and/or; 量纲必须出自字典
  for: 5m                           # 持续时长才 firing (防抖)
  severity: minor
  hysteresis: 0.5                   # 恢复阈值偏移: 恢复条件 = fws.deltat >= 4 + 0.5
  annotations:
    summary: "CDU 一次侧温差过低"
    runbook: "rb/cdu-deltat-low"    # M2 知识库键 (T19), 先占位
```

规则语义：

- `expr` 中点位是**相对路径**（同点表），实例化时拼接资产路径；引用多个点位允许
  （如 `fws.supply.temp - fws.return.temp`），但只能引用同一资产实例的点
- `for`：条件连续满足该时长才进入 firing；数据缺失（stale/offline）**不**视为满足
- `hysteresis`：缺省 0（恢复 = 条件不满足）；方向由比较符自动推导
- 站点级规则（appliesTo: site，如 PUE 越限）允许，expr 引用站点级派生量（spec-002 §9）
- 状态类规则直接用枚举：`expr: "status == 3"`（fault）→ 推荐每类型标配

---

# 4. 告警状态机

```text
                 ┌──────────── maintenance 抑制 ────────────┐
                 ▼                                          │
  (条件满足 for) ──→ firing ──(人工 ack)──→ acked            │
       │              │                      │              │
       │              └──(恢复条件满足)──────┴──→ resolved ──┘
       │
  suppressed（maintenance / 父级 offline 屏蔽，不通知、仍记录）
```

| 状态 | 进入条件 | 通知 |
|------|---------|------|
| `firing` | expr 满足且持续 ≥ for | 按 §5 路由 |
| `acked` | 人工确认（记审计） | 停止重复通知，保留升级（escalation 未决 Q2） |
| `resolved` | 恢复条件（含 hysteresis）满足 | 发 resolved 通知 |
| `suppressed` | 资产或祖先处于 maintenance；或父级设备 offline 引起的派生告警 | 不通知，事件仍入库 |

- **去重键** = `(rule.name, 实例化路径)`；同键 firing 期间不重复发 firing 事件
- **父级屏蔽**：设备 offline（status=5）时，其子树点位的 stale 类告警合并为单条
  设备级告警，避免告警风暴
- 全部状态变迁发 CloudEvents（§1.2）并持久化 PostgreSQL（spec-006 §1.1）

---

# 5. 通知路由

```yaml
kind: NotifyRoute
metadata: { name: default }
spec:
  routes:
    - match: { severity: critical }
      notify: [webhook-oncall, sms-duty]     # 通知器实例名
      repeat: 15m                            # firing 未 ack 期间重复间隔
    - match: { severity: major, domain: cooling }
      notify: [im-cooling-group]
    - match: { severity: [minor, info] }
      notify: [im-ops-channel]
      repeat: never
```

- 通知器是插件（webhook/email/IM），配置归 cios-alarm；M1 首批交付 webhook + email
- `domain` 取 spec-001 §3 域派生属性；`match` 支持 severity/domain/type/路径 glob
- cios-alarm 不依赖 cios-core 存活（spec-006 §1.1 爆炸半径约束）

---

# 6. 未决问题

| # | 问题 | 阻塞 |
|---|------|------|
| ~~Q1~~ | ~~告警规则表达式引擎选型：自研比较器 vs 嵌入 PromQL 子集~~ → **已决 L66（2026-06-13）：自研比较器，跑在 NATS 遥测活流上（安全关键 + 低时延 + TSDB 退化仍触发）；触发改用 PromQL 的信号见 L67 末** | ✅ 解锁 cios-alarm（PRMT-020） |
| Q2 | escalation 策略（acked 超时未处理是否升级、升级链）| M2 排班（T18）就绪后定 |
| Q3 | 告警事件保留期（PG 内全量 vs 滚动归档对象存储） | 存储预算（T3 规模包络） |
