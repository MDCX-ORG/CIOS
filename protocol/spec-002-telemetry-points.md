# CIOS Spec 002 — Telemetry Points

> 状态：**v1.0（冻结，2026-06-13 评审会）** · 对应 Implementation Plan D0.2，依赖 Spec 001 v1.0
> +§8bis（2026-07-16，additive 附录，不动冻结正文）：D8 risk_class 首批控制点清单（L108，Yuri 逐批确认）
> v1.0：冻结；D19–D21 字典变更追认（smoke 5 级 / side +input/output 与风侧复用 / sensor 宿主类型，详见 LOCKED L56）
> v0.5：新增 `protocol/units.yaml` 为单位换算权威源（线性换算 factor+offset；
> 点表 `unit_in` 必须出自该表，conformance V2 据此校验）——厂家无关开发策略落地件之一
> v0.6：采样率为**配置不为代码**（L52：默认 电气 1s/温度流量 10s/状态 COV+60s 心跳/GPU 遥测 1–5s，
> 点表新增可选 `interval` 字段逐点覆盖）；单位/量纲可经 ext.d 片段扩展（L54）；
> 性能基线口径=含 GPU/BMC 带内遥测（L51，首项目 >30 pod ⇒ 单站设计 ~100 万点）
> v0.2：量纲 `valve` 改名 `opening`；量纲补遗 10 项；loop 冗余索引；派生量地址；access/source/risk_class
> v0.3：GPU/算力采集分级定案（node 强制 + gpu subsystem 可选，对齐 NV 管理软件输出）
> v0.4：水路 temp 缺 side 定为 **error**、flow/press 保持 warning（L47/D13）；
> 站点级地址仅限 host=site 的派生量，普通量纲必须挂资产（L48/D12）
> 测点是资产路径的叶子，点位地址 = 资产路径 + 位置段 + 量纲。

---

# 1. 点位地址语法

```ebnf
point     ::= asset_path ( "." location )* "." quantity
asset_path ::= Spec 001 §2 定义的资产路径
location  ::= loop | side | phase            ; 可选限定段, 按需级联
loop      ::= ("fws" | "tcs") [0-9]?         ; 一次/二次侧水路; 可带冗余回路索引 (fws0/fws1)
side      ::= "supply" | "return"            ; 供水 / 回水
phase     ::= "l1" | "l2" | "l3" | "n"       ; 三相电 + 中性线
quantity  ::= 量纲字典(§3)中的标识符
```

示例：

```text
site01.pod002.cdu000.fws.supply.flow     pod002 第一个 cdu、一次侧供水流速
site01.pod002.cdu000.fws.return.temp     同上回水温度（ΔT 计算依据）
site01.pod002.rack000.tcs.supply.press   pod002 第一个 rack、二次侧供水压力
site01.pod003.busbar000.tou005.amp       供 rack005 的 TOU 总电流
site01.pod003.busbar000.tou005.l1.amp    同上 L1 相电流
site01.pod003.busbar000.tou005.status    TOU 状态
site01.chiller000.pump002.rpm            chiller 水泵转速
site01.switchgear000.meter000.energy     站级电表累计电量
site01.bess000.battery002.cell117.resistance   单体电池内阻
site01.pod000.tank003.leak               液槽漏液检测
```

规则：

- 位置段顺序固定：`loop → side → phase`，缺省即整体量（如 `tou005.amp` 表示总电流）
- 双回路冗余场景用 loop 索引：`fws0/fws1`；单回路缺省写 `fws`
- **水路温度（loop 段存在时的 temp）强制带 side**（supply/return），缺失 = **校验 error 拒绝**（L47）；
  flow/press 建议带 side，缺失报 warning；无 loop 段的 temp（如 gpu0.temp）不受此限
- 点位地址全小写，量纲必须来自 §3 字典，未注册量纲拒绝采集入库
- **词汇互斥铁律**（同 Spec 001 §1）：类型名 ∩（位置段 ∪ 量纲名）= ∅，CI 对三张表做交集检查

---

# 2. 位置段定义

| 段 | 取值 | 含义 |
|----|------|------|
| loop | fws[0-9]? | Facility Water System，一次侧（设施水）；索引表示冗余回路 |
| | tcs[0-9]? | Technology Cooling System，二次侧（技术冷却液；**含 AC 系列浸没液回路**，tank 侧统一用 tcs，不另设 oil 段） |
| side | supply | 供水/进水 |
| | return | 回水/出水 |
| phase | l1 / l2 / l3 | 三相电相位 |
| | n | 中性线 |

新增位置段 = 修订本表，PR 评审。

---

# 3. 量纲字典（Quantity Dictionary）

**权威源是 `spec/quantities.yaml`（机器可读），本表由其生成。**

| quantity | 单位（入库强制） | 类型 | 说明 |
|----------|----------------|------|------|
| temp | °C | gauge | 温度 |
| flow | L/min | gauge | 流速 |
| press | kPa | gauge | 压力 |
| level | % (0-100) | gauge | 液位 |
| humidity | %RH | gauge | 湿度 |
| amp | A | gauge | 电流 |
| volt | V | gauge | 电压 |
| power | W | gauge | 有功功率 |
| reactive | var | gauge | 无功功率 |
| pf | ratio (0-1) | gauge | 功率因数 |
| freq | Hz | gauge | 频率 |
| energy | kWh | counter | 累计电量（计费源） |
| soc | % (0-100) | gauge | 电池荷电状态 |
| resistance | mΩ | gauge | 电池单体内阻（健康度/预测核心信号） |
| rpm | rpm | gauge | 转速（风扇/泵） |
| opening | % (0-100) | gauge | 阀门开度（原 valve，因词汇互斥改名） |
| status | enum (§5) | gauge | 设备状态 |
| util | % (0-100) | gauge | 利用率（GPU/CPU/Mem） |
| mem | bytes | gauge | 内存/显存用量 |
| clock | MHz | gauge | 时钟频率（GPU 降频检测） |
| ecc | count | counter | ECC 错误累计（M4 预测信号） |
| bandwidth | bps | gauge | 链路流量（NVLink/网络端口） |
| runhours | h | counter | 累计运行小时（M2 按计量维护触发） |
| volume | L | counter | 累计水量（补水计量 → WUE） |
| vibration | mm/s | gauge | 振动速度（M4 预测性维护，M2 起埋点） |
| leak | enum (0=干燥, 1=漏液) | gauge | 漏液检测（液冷场景关键；定位型传感器另配 `leakpos` 米数） |
| fire | enum (0=正常, 1=告警) | gauge | 消防告警信号（B1：测点级接入） |
| door | enum (0=关, 1=开) | gauge | 门磁状态 |
| smoke | enum (0=正常, 1=告警) | gauge | 烟感 |

- 单位换算在 gateway 完成（设备报 m³/h 也以 L/min 入库），**库内永远是标准单位**
- 新量纲 = 修订本表；驱动上报未注册量纲时整点丢弃并告警
- 传感器型号差异由**点表 + 量纲字典转义**吸收（B3 已决）：任何厂家的传感器，
  经点表映射为标准量纲后系统无差别处理，选型不影响 schema

---

# 4. 阀门建模约定

```text
简单阀门（仅开度/通断信号） → 测点：cdu000.tcs.opening = 开度%
带控制器/多信号的阀组       → 资产：chiller000.valve000 下挂 opening / status / amp 等测点
```

判据：信号数 ≥2 或需要被控制指令寻址 → 建资产；否则只是测点。

---

# 5. 状态枚举（status 标准编码）

| 值 | 含义 |
|----|------|
| 0 | ok（正常运行） |
| 1 | standby（待机/备用） |
| 2 | warning（带病运行） |
| 3 | fault（故障停机） |
| 4 | maintenance（维护中，告警抑制） |
| 5 | offline（失联） |

- 所有设备状态统一映射到此 6 值编码（设备私有状态码在点表中映射）
- 私有状态详情保留在 `status_raw` 测点（原始值），便于排障

---

# 6. 点表（Point Map）：协议地址 → 点位地址

点表是型号包的一部分（Spec 001 §4.5），将驱动协议地址绑定到点位：

```yaml
kind: PointMap
metadata:
  name: cdu-vendorX-m300
  driver: modbus
  appliesTo: cdu
spec:
  points:
    - point: fws.supply.flow        # 相对路径, 实例化时拼接资产路径
      register: 30021
      type: holding, float32, be
      scale: 0.1                    # 原始值×0.1
      unit_in: m3ph                 # 设备单位, gateway 换算到 L/min
    - point: fws.supply.temp
      register: 30023
      scale: 0.01
    - point: tcs.opening            # 可控点必须显式标注
      register: 40011
      access: rw                    # ro(默认) | rw → 决定 Set 是否可用 (§8)
      risk_class: a                 # a|b|c 控制风险分级 (spec-006 §5.4); rw 点必填
      limits: { min: 0, max: 100 }  # a 类点安全限幅, 超限拒绝
    - point: status
      register: 30001
      enum_map: { 1: 0, 2: 1, 16: 3 }   # 厂商码 → 标准码(§5)
```

点定义字段：

- `access: ro|rw`：缺省 ro；**M0/M1 编写点表时即标注可写性**，避免 M4 返工
- `source: measured|virtual|derived`：缺省 measured；`virtual` = 无监测能力设备的手工赋值点；
  `derived` 点由 recording rule 生成（§9），不出现在点表
- 新设备型号 = 新点表文件，不改代码；点表必须通过 conformance test
  （量纲合法、单位可换算、枚举映射完整、rw 点有回读地址）

---

# 7. Prometheus / 北向映射

点位地址是 CIOS 内部唯一标识；对外暴露时投影为 Prometheus 序列：

```text
指标名 = cios_<quantity>_<unit>
标签   = path(资产路径) + 位置段展开 + 路径分段展开
```

```text
site01.pod002.cdu000.fws.supply.flow
  ↓
cios_flow_lpm{
  site="site01", pod="pod002", cdu="cdu000",
  path="site01.pod002.cdu000",
  loop="fws", side="supply",
  asset_type="cdu", domain="computing",
}
```

- `domain` 标签由顶层类型派生（Spec 001 §3）；`asset_type` 标签 = 路径**末节点（叶子设备）类型**——两者来自路径两端，并存不冲突（v1.0 澄清，PRMT-010 实现对齐）
- Grafana/第三方系统按标准 Prometheus 语义直接消费，无需理解 CIOS 路径模型

## 算力采集分级（已决 D3）

数据源以 NVIDIA 管理软件（DCGM 等）的实际输出为准，按两级采集：

| 级别 | 对象 | 采集要求 | 内容 |
|------|------|---------|------|
| **强制** | node（服务器级） | 所有站点必采 | 整机功率、温度、util、mem、状态、带内/带外可达性 |
| **可选** | gpu + subsystem 细分 | 型号包按需开启 | 每卡 util/mem/clock/ecc/bandwidth，细分用 `subsystem` 标签（sm/mem/nvlink/...），不进点位段 |

`subsystem` 取值跟随 DCGM 字段表，随 DCGM 驱动版本发布——NV 软件输出变化时只更新映射表。

---

# 8. Query / Set：路径上的统一读写动词

点位地址空间支持两个动词，digital twin 的查看与控制共用同一寻址：

```text
Query <path>          读：实时值 / 历史 / 资产属性
Set   <path> <value>  写：下发控制 / 应用配置
```

```text
cios query site01.pod002.cdu000.fws.supply.flow           # 当前值
cios query 'site01.pod003.busbar000.tou*.amp' --watch     # 实时滚动, 支持 glob
cios query site01.pod002.cdu000.fws.return.temp --last 1h # 历史
cios query site01.pod002.cdu000 --points                  # 列出该资产全部测点

cios set site01.chiller000.drycooler000.fan003.rpm 800    # 控制点写入
cios set site01.pod000.cdu000.tcs.opening 45              # 阀门开度
cios set site01.pod000 --attr lifecycle=maintenance       # 资产配置变更
```

Set 安全规则（强制）：

- 点表中 `access: rw` 的点才可写，默认全部 `ro`；点表在 M0/M1 编写时即标注可写性
- 每次 Set 记审计日志（who/when/path/old/new）；高危点（断路器分合、泵启停）要求双人审批链
- API 形态：`GET /v1/points/{path}`、`PUT /v1/points/{path}:set`；digital twin UI 的点击控制走同一 API
- 写入结果异步确认：Set 返回 command id，执行回读（readback）校验后才标记成功

## 8bis. risk_class 首批控制点清单（D8 → L108，2026-07-16，additive 附录）

> Yuri 2026-07-16 逐批确认（D8 收口）。分级语义 = spec-006 §5.4：**A** 审批链+回读+全量审计｜**B** 单人授权+回读+审计｜**C** 单人+审计。
> **默认 ro 恒定**：未列入本表（及后续同流程增补）的点一律 `access: ro`；rw 增补必须走 D8 同款逐点确认 + 新 L。
> A 级动作对齐电力操作票语义。点表（pointmaps/*.yaml）落地时逐点标注 `access: rw` + `risk_class`。

| 点位 | 动作 | 级 |
|---|---|---|
| `chiller.compressor.status` | 压缩机启停 | A |
| `chiller.fws.supply.temp` (setpoint) | 供水温度设定 | A |
| `cdu.pump.rpm` (setpoint) | 二次泵转速 | A |
| `chiller.valve.opening` / 环管 PICV/DPBV | 一次侧阀开度 | A |
| `switchgear.breaker.status` | 断路器分合闸 | A |
| `bess.pcs.power` (setpoint) | 充放电功率调度 | A |
| `genset.status` | 发电机组启停 | A |
| `ups.status` (模式/旁路) | UPS 模式切换 | A |
| `transformer` 分接/投切 | 变压器操作（硬件多为只读，预留；不可控硬件标 N/A） | A |
| `drycooler.fan.rpm` (setpoint) | 干冷风扇转速 | B |
| `cdu.valve.opening` (pod 内二次侧) | 二次侧阀开度 | B |
| `tou.status` | TOU 负载通断 | B |
| `pdu.status` | PDU 回路通断 | B |
| `node.status` (IPMI) | 节点开关机 | B |
| `gpu.clock` (DCGM 上限) | GPU 限频 | C |
| `*.status = maintenance` | 维护模式标注 | C |

---

# 9. 派生量（有地址、有公式、口径统一）

派生量进量纲字典（标 `derived: true`），**挂在宿主资产路径上获得地址**，
由 recording rule 统一实现，禁止各处自算。Query 对派生点与实测点无差别。
**站点级地址（site 代号后直接接量纲，如 `site01.pue`）仅允许 host=site 的派生量；
普通量纲必须挂在资产路径上，`site01.temp` 之类非法（L48/D12）**。PUE 只有站点级，
不定义 pod 级 PUE：

| 派生点地址（示例） | 公式 |
|------------------|------|
| `site01.pue` | Facility Load ÷ IT Load |
| `site01.wue` | 补水量（volume 差分）÷ IT 用电量 |
| `site01.pod002.cdu000.fws.deltat` | `return.temp − supply.temp` |
| `site01.pod002.cdu000.fws.heat`（kW） | `flow × ΔT × 比热 × 密度 / 60`（介质参数按 loop 取值） |
| `site01.chiller000.cop` | 制冷量 ÷ 耗电功率 |
| `site01.itload` | Σ tou.power（或 pdu.power，按站点计量配置） |
| `site01.facilityload` | 站级 meter.power |

```text
cios query site01.pue --last 24h     # 派生点用法与实测点完全一致
```

【待补：BESS 充放电效率公式】

---

# 10. 未决问题

（当前无未决问题）

已决：

- ~~P2 GPU 细分~~ → `subsystem` 标签（不进点位段）；node 级强制采集、gpu 级可选，
  以 NV 管理软件输出为准（v0.3，D3）
- ~~P3 补水表~~ → 新增 watermeter 类型（Spec 001 §4.2），WUE = watermeter.volume 差分 ÷ IT 用电（v0.3，D4）

- ~~P1 浸没液回路~~ → tank 侧统一用 `tcs`，不新增 oil 段（示例：`site01.pod001.tank003.tcs.supply.temp`）
- ~~P4 控制点~~ → 复用点位地址 + Query/Set 统一动词（§8）；点表标注 `access: ro|rw`，写操作带审批与回读确认
- ~~S2 词汇冲突~~ → `valve` 量纲改名 `opening`；词汇互斥铁律进 CI（v0.2）
- ~~S5 冗余回路~~ → loop 段可带索引 fws0/fws1（v0.2）
- ~~S7 派生量地址~~ → 进量纲字典、挂宿主路径（§9，v0.2）
- ~~B3 传感器选型~~ → 型号差异由点表+量纲字典转义吸收，选型不阻塞 schema（v0.2）
