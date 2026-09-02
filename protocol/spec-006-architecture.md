# CIOS Spec 006 — Modules, Deployment & Interface Architecture

> 状态：Draft v0.3 · 2026-07-16 · 依赖 spec-001/002，被 spec-003/004/005 细化
> 回答四个问题：有哪些模块、放在哪（边缘/云）、接口怎么定、冗余/验证/安全怎么做。
> 前提决策：边缘双机可选单机起步；云端 Set 分级允许；上云 = 降采样 + 全量事件。
> v0.2：**多级 Gateway 架构**（pod 级 ARM 预装 + site 级汇聚，逐级防火墙放行，D6/D7）。
> v0.3：**站级服务部署形态锁定（D9 → L104，2026-07-16）**——站级/云端服务 = 云端 x86（§1.2bis）；云部署安全硬前置（§5.0bis）。additive，无破坏性变更。

---

# 1. 模块清单与部署位置

## 1.1 边缘（Site Plane，每站点）

| 模块 | 职责 | 交付里程碑 |
|------|------|-----------|
| **cios-gateway** | 驱动宿主（插件进程）、点表映射、单位换算、入口校验（§4 L1/L2）、本地 WAL 缓冲、**控制执行器**（Set 落地与回读）。**多级部署**（§1.4） | M1 |
| **cios-core** | 站点 API（REST/gRPC 唯一入口）、CMDB 站点实例、模板实例化、Query/Set 编排与审批、RBAC 执行 | M1 |
| **cios-alarm** | 告警规则引擎（消费总线）、告警状态机、通知器（webhook/email/IM 插件） | M1 |
| **cios-rules** | 派生点计算（recording rules → spec-002 §9） | M1 |
| **cios-sync** | 边↔云同步代理：遥测/事件上行（store-and-forward）、配置/型号包下行 | M1 |
| **cios-web** | 站点 Dashboard（静态 + 嵌入 Grafana panel） | M1 |
| VictoriaMetrics | 边缘 TSDB（单机版），原始数据 90 天 + 5m 降采样 | M1 |
| PostgreSQL | CMDB / occupancy / 告警 / 工单 / 审计 | M1 |
| NATS JetStream | 站内总线 + 上云缓冲（≥7 天滞留） | M1 |

> 设计约束：**单机可跑全栈**（docker compose / systemd），双机为可选 HA 模式（§3）。
> cios-alarm 不依赖 cios-core 存活——采集→告警链路在 core 故障时仍然工作（爆炸半径隔离，§3.4）。

## 1.2 云端（Fleet Plane）

| 模块 | 职责 | 交付里程碑 |
|------|------|-----------|
| **fleet-ingest** | NATS hub（接收各站点 leafnode）、遥测写入云端 TSDB、同步对账（§4 L5） | M1 |
| **fleet-api** | 多站点 API（站点 API 的同构超集）、全局 RBAC/OIDC、租户模型 | M1 基础 / M3 完整 |
| **fleet-registry** | 站点注册表（site 代号分配）、**型号包分发**（模板+点表+告警规则版本化推送）、OTA 通道 | M1 |
| fleet-web | 多站点汇聚 Dashboard | M2 |
| fleet-billing / fleet-cost | 计费引擎 / 成本引擎（消费计费级 energy 原始流） | M3 |
| portal | 客户门户 | M3 |
| fleet-analytics | Revenue/EBITDA/ROI | M3/M4 |
| ai-ops | AI Assistant（工具=fleet-api/CLI）、预测（容量/故障）、能源优化策略 | M4 |
| VM 集群 / PostgreSQL / 对象存储 | 多站点 TSDB（RF=2）、全局库、备份与报告归档 | M1 |

## 1.2bis 站级服务器部署形态（已决 D9 → L104，2026-07-16）

```text
方向锁定：站级服务（cios-core / VictoriaMetrics / PostgreSQL / NATS / cios-apigw）
         部署于云端、x86 架构服务器。
边缘侧：  pod gateway 维持 ARM / Linux 随 pod 预装（L42/D7 不变）；
         断网自治要求（ADR-2，≥7 天）由 pod/site 侧 WAL + JetStream 滞留承担。
未锁定：  具体 sizing / BOM / TCO / 云厂商 / 双机放置 —— gated on D10 实测
         （benchmark PRMT，advisory-only）+ T3 规模包络，证据到位后另拍。
硬前置：  customer-facing 云部署前必须完成 §5.0bis 三项安全硬化。
```

## 1.3 数据驻留（上云粒度，已决）

```text
留边缘：全量原始遥测（90 天）、原始审计
上云：  1m 降采样遥测 ＋ 全量告警/事件/工单 ＋ 计费级原始流（energy/占用区间/membership）
        ＋ CMDB 全量镜像 ＋ 边缘 PG 的 WAL 备份
按需：  云端可向站点发起历史原始数据回查（fleet-api 透传到站点 API）
```

## 1.4 多级 Gateway 架构（已决 D6/D7）

```text
┌─ Site ──────────────────────────────────────────────┐
│  site gateway + core/alarm/VM/PG/NATS（站级服务器）   │
│  采集对象: chiller / switchgear / bess / 站级网络     │
│      △ 由 Site 级防火墙放行（pod→site 单向）          │
│  ┌─ Pod000 ────────────┐  ┌─ Pod001 ────────────┐   │
│  │ pod gateway (ARM)   │  │ pod gateway (ARM)   │   │
│  │ 采集: tank/node/GPU │  │ ...                 │   │
│  │  cdu/ups/busbar/tou │  │                     │   │
│  │ △ Pod 防火墙放行     │  │                     │   │
│  └─────────────────────┘  └─────────────────────┘   │
│      △ 由上一级（Site→云）防火墙放行，只出站           │
└─────────────────────────────────────────────────────┘
```

- **pod gateway**：ARM 级硬件、Linux，**随 pod 出厂预装**（型号包内含 gateway 镜像+点表+证书引导）
  → pod 接入站点 = 上电 + 防火墙放行 + fleet-registry 认领，即插即用
- **site gateway**：跑在站级服务器，采集站级设备，同时作为 pod gateway 的 NATS 汇聚上级
- 放行原则（D6）：**每级 gateway 由其上一级防火墙显式放行**，pod 内设备网对 site 不可见，
  site 设备网对云不可见——天然纵深防御，分级与 §5.2 零入站原则一致
- pod gateway 与 site gateway 是同一个 `cios-gateway` 程序（Go 单二进制 arm64/amd64），
  差别只在配置（驱动清单+上级 NATS 地址）
- 双机 HA（§3.2）只适用 site 级；pod gateway 单实例（故障域=单 pod，WAL 兜底+快速换件恢复）

---

# 2. 模块接口定义

## 2.1 接口形态总表

| 接口 | 形态 | 契约 | 规范 |
|------|------|------|------|
| 南向（gateway↔driver） | gRPC over unix socket（go-plugin 模式） | protobuf：Init/Discover/Collect/Subscribe/Write/Health | spec-005 |
| 站内总线 | NATS JetStream | protobuf（遥测批）/ CloudEvents（事件） | 本文 §2.2 |
| 站点北向 API | REST `/v1`（+ 内部 gRPC） | OpenAPI 3.1 | spec-004 |
| 云端北向 API | REST `/v1`，**站点 API 的同构超集** | OpenAPI 3.1 | spec-004 |
| 边↔云 | NATS leafnode over TLS，**边缘只出站** | JetStream stream mirror + KV（配置下行） | 本文 §2.3 |
| Web/CLI/AI → 系统 | 一律走 REST API，无后门 | — | ADR-4 |

**API 同构原则**：`cios` CLI 指向站点或云端皆可用——
`cios query site01.pod002...` 在站内直达，在云端自动路由到对应站点资源或云端副本。
fleet-api 仅追加多站点/租户/计费资源，不改变已有资源语义。

## 2.2 NATS Subject 命名规范（站内 + 上云同构）

```text
cios.tlm.<site>.<top_asset>      遥测批（protobuf, 按顶层资产分区保序）
                                 例: cios.tlm.site01.pod002
cios.evt.<site>.alarm            告警事件（CloudEvents, spec-003）
cios.evt.<site>.lifecycle        资产生命周期/occupancy 变更
cios.evt.<site>.audit            审计事件
cios.cmd.<site>.<gateway_id>     控制命令下行（含 TTL+nonce, §5.4）
cios.cmdres.<site>               控制结果/回读上行
cios.cfg.<site>                  配置版本通知（型号包/告警包/点表更新）
```

- 遥测 payload：`Sample{path, location, quantity, ts, value, quality}` 批量帧
- 所有 subject 以 site 隔离，云端按 NATS account 实施站点间隔离（§5.2）

## 2.3 边↔云同步协议

```text
上行（JetStream source/mirror，至少一次送达 + 幂等去重）
  tlm-1m   : 1m 降采样流
  evt      : 事件全量流
  billing  : energy 原始 + occupancy/membership 区间（计费审计源）
下行（JetStream KV watch）
  cfg      : 型号包/点表/告警规则包（版本化, 边缘原子切换+回滚）
  identity : RBAC/用户同步（站点离线时本地缓存仍可鉴权）
控制（request-reply over leafnode）
  cmd      : 云端 Set → 站点 core 审批链 → gateway（分级规则 §5.4）
```

- 断链：JetStream 边缘滞留 ≥7 天，恢复后按 sequence 续传；去重键 `(path, ts)`
- 对账：每 5m 窗口生成 `manifest{count, checksum}` 随流上行，fleet-ingest 校验，缺口触发补传（§4 L5）

---

# 3. 冗余措施

## 3.1 边缘单机模式（起步形态）

- 进程级自愈：systemd/容器重启策略 + 健康检查（所有模块暴露 `/healthz`）
- gateway WAL：NATS 不可用时驱动采样落本地磁盘，恢复后回放（采集链路最后防线）。
  **7 天无损容量（L65 定稿）**：单 pod ≥3 万点全量缓冲 7 天原始体量 ≈120–700 GB，故
  ① 7 天无损由**出厂磁盘容量**保证（非代码默认值）；② WAL 上限为**配置项**，默认按出厂
  WAL 分区设定；③ **强制 WAL 帧 gzip 压缩**（Prometheus 文本 ~8–10×），压缩后 7 天
  ≈15–80 GB，落 **128–256 GB SSD 分区**为出厂硬件包络。不采纳断网降采样（与 B.4 无损口径冲突）
- 数据备份：PG WAL 持续上云 + VM 快照每日上云（对象存储）→ 单机硬件损毁 RPO ≤5min（计费级流实时上云，损失≈0）
- 元监控：站点自身指标（`cios.sys.*` 命名空间）同样上云，云端对站点做存活告警

## 3.2 边缘双机模式（可选 HA）

```text
形态: active-standby 全栈对（VIP 切换）
  PG   : 流复制（同步模式），失败自动提升
  VM   : vmagent 双写两实例（查询走 VIP）
  NATS : 2 数据节点 + 云端仲裁（避免边缘凑 3 节点）
  采集 : 租约式单活——同一设备同一时刻只有一个 gateway 在轮询
        （防双 Modbus 会话冲击设备）；租约失效 10s 内接管
  控制 : 严格单活 + fencing——切换瞬间禁止 Set 执行，防双写设备
目标: 故障切换 ≤60s，切换期间采集缓冲不丢
```

## 3.3 云端

- k8s 多副本 + 多 AZ；VM 集群 RF=2；PG 主从自动切换；NATS 3 节点 raft
- 云端整体故障 = 站点无感（边缘自治 ≥7 天，ADR-2）

## 3.4 爆炸半径矩阵（设计验收用）

| 故障 | 影响 | 不受影响 |
|------|------|---------|
| 云端整体宕 | 多站点视图/计费暂停 | 站点采集/告警/控制/Dashboard 全部正常 |
| cios-core 宕 | API/UI/Set 不可用 | 采集→存储→**告警→通知** 正常 |
| NATS 宕 | 总线中断 | gateway WAL 兜底采集不丢；core 仍可直查 VM |
| gateway 宕 | 该协议区采集中断（双机则接管） | 其他 gateway、已存数据、API |
| 边↔云断链 | 上云延迟（≤7 天可恢复） | 站点全功能 |

---

# 4. 数据验证措施（五层）

| 层 | 位置 | 检查项 | 不合格处理 |
|----|------|--------|-----------|
| **L1 入口校验** | gateway | 量纲已注册、单位可换算、量程（per-quantity min/max，点表可覆写）、变化率突刺、时间戳合理（拒绝未来 >5s）、counter 单调（识别复位）、枚举映射完整 | 标记 quality=suspect 或丢弃+计数告警 |
| **L2 路径校验** | gateway | path 存在于 CMDB 且 lifecycle 为 active/maintenance | 进**隔离区**（不丢弃），告警提示未注册设备 |
| **L3 质量标记** | 全链路 | 每样本带 `quality: good\|stale\|suspect\|substituted`；派生量继承输入最差质量（输入 suspect → PUE suspect） | 质量标签随 Prometheus 投影暴露 |
| **L4 交叉验证** | cios-rules | 电量平衡：父表 ≈ Σ子表（容差 ±2%，告警）；Set 后回读一致性；设备时钟漂移监控（设备 ts vs gateway ts） | 偏差告警 + 计费数据标记待核 |
| **L5 同步完整性** | fleet-ingest | 5m 窗口 manifest（count+checksum）对账、缺口检测自动补传、计费流 gap 报告 | 对账失败告警；计费缺口走文档化插值规则并留审计 |

计费级附加规则：energy 原始值不可变存储；账单永远可从原始流重算；任何替代/插值值 quality=substituted 且入审计。

---

# 5. 接口安全保障

## 5.0bis 云部署安全硬前置（L104 登记，2026-07-16）

> 2026-07-16 代码扫描（local archive `docs/archive/closeout/CODE-SCAN-2026-07-16.md`）实证下列三项与本 spec §5 要求的差距。
> **任何 customer-facing 云部署（§1.2bis）前必须修复**；lab/edge 环境（127.0.0.1 绑定）可暂容忍。

| # | 差距 | 要求 |
|---|------|------|
| H1 | apigw 在 STS+PDP 均未配置时默认 pass-through（不 fail-closed） | apigw 必须与 cios-core 同款 fail-closed 启动门：无鉴权配置 → 拒绝启动，显式 opt-out 才可放行 |
| H2 | 组件间 mTLS（§5.1）未实现（无 server-side TLS / client-cert 校验；deploy 无终端） | 按 §5.1 CA 层级落地组件间 mTLS，或经架构者批准的等价信道（service mesh / 私网 + 网络策略），登记为 L |
| H3 | `X-CIOS-Tenant` 在 gateway→core 边界被无条件信任（注释声称 mTLS 保护，实际无） | 随 H2 收口；H2 落地前该边界必须有等价网络隔离保证 |

## 5.1 身份体系（一切接口先有身份）

```text
fleet root CA
 └─ site intermediate CA（每站点）
     └─ 组件证书（cios-core/gateway/...，自动签发+90 天轮换）
身份格式: cios://<site>/<component>，mTLS 双向认证覆盖所有组件间通信
```

## 5.2 分区与最小暴露

- **边缘零入站**：站点不开任何入站端口，边↔云为出站单向 TLS（443），NAT/防火墙友好
- 云端 NATS 按 **account 隔离站点**：site01 的凭据无法读写 site02 的 subject
- 驱动插件：unix socket 通信、独立低权限用户、设备凭据按驱动最小可见
- OT/IT 网络分区（T24）：gateway 双宿，设备网默认 deny-all，仅放行协议端口

## 5.3 北向 API 安全

- 人类：OIDC（SSO）；机器：scoped API token（可吊销、有效期、最小范围）
- RBAC = 角色 × 资源范围（**路径 glob**，如 `site01.chiller*`）——spec-004 细化
- 限流 + 全量审计（who/when/what/from-where），审计流上云不可篡改

## 5.4 Set 控制链安全（最高防护等级）

点表为每个 rw 点标注风险级（spec-002 点表增加 `risk_class` 字段）：

| 级别 | 示例 | 发起范围 | 审批 |
|------|------|---------|------|
| **A 低危** | 风扇转速、阀门开度（限幅内） | 站内 + 云端 | 单人 + 审计 |
| **B 中危** | CDU 模式切换、采样配置 | 仅站内 | 单人 + 审计 |
| **C 高危** | 断路器分合、泵启停、UPS 操作 | 仅站内 | **双人审批** + 审计 |

- 命令报文含 `TTL + nonce`（防重放）、限幅校验（A 类点定义安全范围，超限拒绝）
- 回读确认强制（spec-002 §8）；双机切换瞬间 Set 熔断（§3.2）
- M4 自动控制（能源优化）走同一链路：AI 只是另一个带 `control:write` scope 的客户端，
  初期输出建议、人工确认执行，验证期后对 A 类点放开自动执行

## 5.5 供应链与更新

- 发布产物签名（cosign），边缘 OTA 验签 + 灰度 + 失败自动回滚
- 型号包（模板/点表/告警包）同样签名分发——点表是能改控制行为的配置，视同代码管理

---

# 6. 未决问题

| # | 问题 | 阻塞 |
|---|------|------|
| A1 | **站级**服务器选型（pod gateway 已决：ARM/Linux 随 pod 预装，具体型号待定） | 部署模式 |
| A2 | NATS 边缘双机 + 云仲裁的网络抖动容忍度需 PoC 验证 | 双机模式 |
| A3 | risk_class 的点位清单（哪些点是 A/B/C）随首批点表确定（D8 开放） | Set 分级落地 |
| A4 | pod gateway ARM 资源预算（驱动数×采样率 → CPU/内存/存储下限），影响硬件选型 | A1/D7 |
