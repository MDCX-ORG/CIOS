# CIOS Spec 001 — Asset Model & Naming

> 状态：**v1.1（2026-07-10，治理评审授权 L101；评审记录 = docs/spec-001-v1.1-upgrade-dossier.md §8）** · 对应 Implementation Plan D0.1 · 治理规则见 docs/spec-governance.md
> v1.1：**经 L101 评审授权的 v1.0 冻结线例外升版**（仍守「只增不改名」）——新增 §5bis 租户与 Org 对象（L84/L98/L99）；§5 Cluster 增必填 `org:` 字段（B1 Cluster ⊂ Org）；新增 §7bis 端口语义（`liquid_in`/`liquid_out`，E3.6a Domain-C）；附录 A.2 增 site `city` 可选属性；新增 §10 Core/Scene Engine 计算边界。**破坏面仅 crn/RBAC 资源名语法（org 段），定义在 spec-004 v1.1 §6bis**——本文 §2 遥测点路径语法**逐字不变**
> v1.0：冻结；附录 A（类型属性 schema，protocol/spec-001-appendix-a-attributes.md）随本体生效；此后破坏性变更禁止
> v0.4：双层身份（路径=位置，serial=物理实体+occupancy）；词汇互斥铁律；三位补零；关系边属性；cell 类型
> v0.5：site 代号规则（城市优先+云端分配）；network 域类型表定稿（fabric/带内/带外三平面）；
> **Cluster 可跨站点**；新增 watermeter 类型
> v0.6：chiller 合法父级 +pod（L46：1:1 配套挂 pod 下、多 pod 共享挂 site 下）；
> 站点级地址仅限 host=site 的派生量（L48）
> v0.7：**分层字典（L54）**——核心表 + `protocol/ext.d/*.yaml` 扩展片段（新 BOM 即可新增词条，
> CI 互斥校验是唯一闸门；片段只增不改，改既有词条走核心表 PR）；cdu 父级 +tank、tou desc
> 放宽（EXT-001 实测）；首批片段 mdc-ext-001（rdhx/ceilair/manifold/pdc/ats + conductivity/h2）

---

# 1. 模型总览

```text
Site（物理场地）
 ├─ pod{n}         算力集装箱 (AC40/AC45/DC40/DC45/未来型号)
 ├─ chiller{m}     HybridChiller
 ├─ switchgear{k}  开关柜
 ├─ bess{j}        场地级储能（含管理系统、电池、PCS）
 ├─ transformer / meter / genset
 └─ coreswitch / router / firewall

虚拟对象（不在路径中）：
 Cluster = n×Pod + m×Chiller + Switchgear + BESS，绑定一个用户/用户组
```

六条铁律：

```text
1. 路径 = 位置身份（槽位/机位），只表达物理包含，终身不可变；
   位置上的物理设备可以更换，路径不变
2. serial = 物理实体身份；serial ↔ 路径 的对应关系由 occupancy 区间记录（§6）
3. 域（computing/cooling/feed/network）不进路径，由顶层类型派生（§3）
4. 供电/冷却/网络连接关系不进路径，用关系边（§7）单独表达
5. 测点（流速/压力/电流/状态...）是路径的叶子，规则见 Spec 002
6. 词汇互斥：类型名 ∩（位置段 ∪ 量纲名）= ∅，三张表交集检查进 CI
```

---

# 2. 命名语法

```ebnf
path     ::= site ( "." node )+
site     ::= citycode seq                   ; 站点代号 = 城市代号 + 序号
citycode ::= [a-z]{2,8}                     ; 城市优先 (如 sgp / kul / hou)
seq      ::= [0-9]{2}                       ; 同城序号, 01 起
node     ::= type index
type     ::= [a-z]+                         ; 小写字母, 来自类型注册表(§4)
index    ::= [0-9]{1,3}                     ; 实例序号
```

site 代号分配（已决 D1）：**城市优先命名，向云平台（fleet-registry，spec-006）注册申请由其分配**，
全局防冲突，一经分配终身不变。示例：`sgp01` 新加坡 1 号站。本文示例的泛指代号 `site01` 亦合法。

序号规则：

- **实例命名 = 类型 + 序号**
- 设备级资产序号 `000` 起、**三位补零**（`pod000`–`pod999`）——字典序永远正确，
  单站同级资产 >99 有真实案例（业内单站 300 微模块）
- 芯片/端口级资产（gpu、cell 等）序号 `0` 起、**不补零**，与硬件枚举严格一致
  （`gpu0`–`gpu7` 对齐 DCGM index，`cell0`–`cell319` 对齐 BMS 编号）；工具层负责自然排序
- 序号在父节点内唯一

身份规则（双层身份）：

```text
路径（位置）                          serial（物理实体）
─────────────────                    ─────────────────
site01.pod000.tank003.node012.gpu3   NVD-H100-8839271
- 绑定槽位/机位，终身不变             - 跟随物理设备一生
- 遥测按路径存储（运维视角连续）       - 备件/保修/故障履历按 serial 追踪
- 换件后路径照旧                      - 换件 = occupancy 记录切换（§6）
```

- **换件**：路径不变、serial 变更，CMDB 自动写一条 occupancy 记录
- **位置裁撤**（物理布局变更）：路径 retire，其序号不回收；新布局用新序号
- **设备移位**：旧位置 occupancy 关闭、新位置 occupancy 开启，serial 的履历跨位置连续
- 同一类型可出现在不同层级（`site01.bess000` 场地级储能 vs `site01.pod001.ups000.bess000`
  UPS 电池模块），由完整路径消歧

示例：

```text
site01.pod000.tank003.node012.gpu0    AC40 浸没 pod 内某 GPU
site01.pod002.rack005.pdu000          DC 系列 pod 内某机柜 PDU
site01.pod000.ups000.bess000          pod 内置 UPS 的电池模块
site01.pod003.busbar000.tou005        pod 母线上供 rack005 的 Tap-Off Unit
site01.chiller002.drycooler000.fan003 chiller 干冷段风扇
site01.switchgear000.breaker004       switchgear 某断路器
site01.bess000.battery002.cell117     场地级 BESS 某电池簇的单体电池
```

查询支持 glob：`cios query 'site01.**.gpu*'`（`*` 单段，`**` 任意深度）。

---

# 3. 域（派生属性，不进路径）

域由**顶层类型**自动派生，用于分类查询与指标标签（Spec 002），不占路径段：

| 顶层类型 | 域 |
|---------|-----|
| pod | computing |
| chiller / watermeter | cooling |
| switchgear / bess / transformer / meter / genset | feed |
| fabricswitch / coreswitch / router / firewall / oobswitch | network |

资产继承其顶层祖先的域：`site01.pod001.ups000` 域 = computing（物理位置决定，
功能横查靠 `asset_type`，如"全站所有 UPS/电表"）。

---

# 4. 资产类型注册表

每个类型定义：合法父类型、必填属性、生命周期。新增类型 = 修订本表（PR 评审），不改代码。
**注册表的权威源是 `protocol/types.yaml`（核心表）+ `protocol/ext.d/*.yaml`（扩展片段）的合并结果
（机器可读，CI 校验词汇互斥），本文档表格由其生成。**
分层规则（L54）：新配置设备/BOM 即可提交扩展片段新增类型/量纲/单位，无需统一审批，CI 互斥与
父子合法性校验是唯一闸门；片段**只允许新增**词条，修改既有词条（父级扩充、改名）仍走核心表 PR 评审。

## 4.1 computing（顶层类型：pod）

| type | 合法父级 | 说明 |
|------|---------|------|
| pod | site | 算力集装箱，属性含 `model: AC40\|AC45\|DC40\|DC45\|...` |
| tank | pod | 浸没液槽（AC 系列） |
| rack | pod | 机柜（DC 系列） |
| node | tank, rack | 服务器节点 |
| gpu | node | GPU，序号对齐 DCGM index（芯片级） |
| cdu | pod, tank | 冷却液分配单元；A32 浸没槽每槽内置 2×CDU（L54/EXT-001） |
| ups | pod | 内置 UPS |
| bess | ups, site | 电池系统（UPS 模块级 / 场地级） |
| pdu | rack, pod | 配电单元 |
| busbar | pod | 母线 |
| tou | busbar | Tap-Off Unit；**序号对齐所供 rack**（tou005 供 rack005），并以 feeds 边显式声明 |
| switch | pod, rack | TOR/管理交换机 |

### Pod 内部结构：由模板配置文件决定（§4.5）

```text
产品族规则：
AC 系列 (AC40/AC45/...)  immersion → pod 含 tank
DC 系列 (DC40/DC45/...)  DLC       → pod 含 rack
```

具体每个型号的内部结构（tank/rack 数量、node/GPU 配置、CDU/UPS/busbar 布局）
**不写死在本规范**，由型号模板文件定义，注册资产时加载实例化。

## 4.2 cooling（顶层类型：chiller）

| type | 合法父级 | 说明 |
|------|---------|------|
| chiller | site, pod | HybridChiller 整机，属性含 `model`。**挂级规则（L46）**：与单一 pod 1:1 配套 → 挂 pod 下（`site01.pod002.chiller000`）；多 pod 共享 → 挂 site 下（`site01.chiller002`）。挂 pod 下时按 §3 域继承为 computing，功能横查用 `asset_type` |
| drycooler | chiller | 干冷段 |
| fan | drycooler | 风扇 |
| pump | chiller | 水泵 |
| compressor | chiller | 压缩机段 |
| valve | chiller, cdu | 带控制/反馈的阀组（简单阀门只作测点 `opening`，见 Spec 002 §4） |
| watermeter | chiller, site | 补水计量水表（已决 D4：闭式系统常态不耗水，但喷淋/补水需计量 → WUE） |

Chiller 内部结构由型号模板文件定义（§4.5），加载时装入。

## 4.3 feed（顶层类型：switchgear / bess / transformer / meter / genset）

| type | 合法父级 | 说明 |
|------|---------|------|
| switchgear | site | 开关柜 |
| breaker | switchgear | 断路器 |
| transformer | site | 变压器 |
| meter | switchgear, tou, transformer, site | 计量电表，**位置由路径决定**（见下） |
| bess | site | 场地级储能系统（含管理系统） |
| battery | bess, ups | 电池簇/电池组 |
| cell | battery | 单体电池（电压/内阻/温度按节监测；芯片级，序号 0 起对齐 BMS 编号） |
| pcs | bess | 功率变换系统 |
| genset | site | 发电机组 |

### Meter 位置语义

电表挂在哪条路径就计量哪一级，计费/能耗归集按路径自动判定计量边界：

```text
site01.switchgear000.meter000          站级/馈线计量
site01.pod000.busbar000.tou002.meter000   rack 级计量（tou002 供 rack002）
```

同一电力路径上出现多级电表时，下级计量是上级的子集，归集引擎按路径层级去重。

## 4.4 network（已决 D2：业务/带内/带外三平面一并建模）

| type | 合法父级 | 说明 |
|------|---------|------|
| fabricswitch | site, pod | 业务算力网交换机，属性 `fabric: roce\|infiniband` |
| coreswitch | site | 核心/汇聚交换机（带内管理网） |
| router | site | 路由器 |
| firewall | site, pod | 防火墙（site 级 + pod 级，对应多级 gateway 放行，spec-006 §5.2） |
| oobswitch | site, pod | 带外管理网交换机（BMC/IPMI 网络） |

- 所有网络类型带 `plane: fabric|inband|oob` 属性，横向查询按 plane 过滤
- pod 内 TOR/管理交换机仍用 computing 域的 `switch` 类型（物理位置决定域，L7），
  同样携带 plane 属性

安防边界（已决 B1）：消防告警、门磁、烟感作为**测点**接入（挂宿主资产或 pod/site）；
门禁/视频的管理功能不在 CIOS 范围，与第三方系统做事件级集成。

## 4.5 型号模板（Template，配置文件）

任何复合资产（pod、chiller、bess、switchgear）的内部结构由模板文件定义，
**加载时读取实例化**，新型号 = 新模板文件，零代码改动：

```yaml
kind: Template
metadata:
  name: ac40-v2
  appliesTo: pod          # pod | chiller | bess | switchgear
  model: AC40
spec:
  structure:
    - type: tank          # AC 系列 → tank
      count: 4
      children:
        - type: node
          count: 12
          children:
            - { type: gpu, count: 8 }
    - { type: cdu,  count: 2 }
    - type: ups
      count: 1
      children:
        - { type: bess, count: 1 }
    - type: busbar
      count: 1
      children:
        - type: tou
          count: 4        # tou 序号对齐所供 rack/tank 位
          children:
            - { type: meter, count: 1 }
    - { type: switch, count: 2 }
```

```yaml
kind: Template
metadata:
  name: hybridchiller-v1
  appliesTo: chiller
  model: HC-XXX           # 【待补: 具体型号编号】
spec:
  structure:
    - type: drycooler
      count: 2
      children:
        - { type: fan, count: 6 }
    - { type: pump, count: 2 }
    - { type: compressor, count: 2 }
```

规则：

- 模板带版本（`ac40-v2`），已实例化的资产记录所用模板版本，模板升级不回溯既有资产
- 模板中的 `count` 是默认值，实例化后允许人工修正个体差异（修正记入变更审计）
- 模板文件与点表（point map，Spec 002）+ 告警规则包 + **USD 模型资产**（digital twin，
  映射规范见 spec-007/ADR-7）构成该型号的完整"型号包"，随产品发布
- CIOS 路径与 USD prim path 同构：`sgp01.pod002.cdu000` ↔ `/sgp01/pod002/cdu000`，
  模板实例化同时驱动 USD 场景装配
- 模板实例化时校验 §4 类型注册表的父子合法性，非法结构拒绝加载

---

# 5. Cluster（虚拟对象，可跨站点）

已决 D5：**Cluster 是纯虚拟定义，Site 是物理定义**。Cluster 不进路径、不绑定单一 site，
成员可以是任意站点物理资产的组合。Cluster 由云端（fleet）持有权威定义，站点缓存本地成员子集：

```yaml
kind: Cluster
metadata:
  name: cluster-alpha          # 全局唯一（fleet 命名空间）
spec:
  tenant: customer-acme        # 绑定用户/用户组 (M3 租户模型)
  org: emea                    # 必填 (v1.1/L99 B1): Cluster ⊂ Org, 恰属一个 Org (§5bis)
                               # 迁移: 既有 Cluster 由 v1.1 迁移自动回填 org: default (L101 D4)
  members:                     # 成员 = 完整路径, 可跨站点
    pods:        [sgp01.pod000, sgp01.pod001, kul01.pod000]
    chillers:    [sgp01.chiller000, kul01.chiller000]
    switchgears: [sgp01.switchgear000]
    bess:        [sgp01.bess000]
  quota:                       # M3 启用
    gpu_count: 384
    power_kw: 1800
```

- Pod 重新分配给其他客户：只改 members 引用，物理路径与历史遥测不变
- 遥测查询时按 membership 注入 `cluster` / `tenant` 标签（查询期 join，不写入原始数据，保证重分配后历史归属可重算）
- 一个物理资产同一时刻只能属于一个 cluster；membership 变更记录生效时间区间（计费按区间切分）

---

# 5bis. 租户与 Organization 对象（v1.1，L84/L98/L99/L101）

三层 scope 轴：**tenant → Organization → site**（L84）。Org 与 Cluster 的关系 = **Cluster ⊂ Org**
（L99 B1：每个 Cluster 恰好挂在一个 Org 之下，见 §5 `org:` 字段）。两对象均为虚拟对象，不进
§2 遥测点路径；在 RBAC 资源名（crn）中的段位见 **spec-004 v1.1 §6bis**。

## 5bis.1 Tenant（租户记录）

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| id | slug，全局唯一 | ✅ | 语法同 site 代号字符集 `[a-z][a-z0-9-]{1,30}` |
| display_name | string | ✅ | 人读名 |
| isolation_tier | enum: `label\|row\|db` | ✅（默认 `label`） | L83 三档分层隔离；**写路径 = 仅 admin、审计型 API、只升不降**（L98(b)） |
| status | enum: `active\|suspended` | ✅ | |
| created_at / updated_at | timestamp | ✅ | |

- **无计费字段**（D34 未拍；拍板后走 spec-010/P683 additive 升版——Yuri 2026-07-10）
- 每次写入记 **append-only 租户审计**（actor + 前后值；L70 posture）
- 存储实现方向 = PG tenants 表（关系侧）+ VictoriaMetrics 原生多租户 `accountID:projectID`
  （遥测侧）——L98(d)；表结构细节由基座 PRMT 依本节定义落地

## 5bis.2 Organization

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| id | 内部标识 | ✅ | |
| tenant_id | 引用 Tenant | ✅ | |
| name | slug，**租户内唯一**（L99 C1） | ✅ | 跨租户重名合法；URL 可读，无需全局注册表 |
| created_at | timestamp | ✅ | |

- site 挂 Org（`site.org`），可改挂，改挂记审计
- **保留名 `default`**：v1.1 迁移为每个既有租户自动创建名为 `default` 的 Org 并回填其全部
  site/Cluster（L101 D4）；`default` 可改名、名下仍有资源时不可删除
- Org 的 crn 段位、glob 语义、双语法过渡期（含 30 天观测关窗）= **spec-004 v1.1 §6bis**

---

# 6. 资产注册与 Occupancy（声明式）

```yaml
kind: Asset
metadata:
  path: site01.pod003
spec:
  type: pod
  model: AC40
  template: ac40-v2           # 加载该模板文件, 自动生成内部资产树 (§4.5)
  vendor: Fog Computing
  serial: FC-AC40-2026-0117   # 注册时同时生成首条 occupancy 记录
  rated_power_kw: 600
  lifecycle: planned          # planned→installed→active→maintenance→retired
```

物理实体与位置的对应由 occupancy 区间维护（换件/移位时自动生成，可手工修正）：

```yaml
kind: Occupancy
spec:
  path: site01.pod000.tank003.node012.gpu3
  serial: NVD-H100-8839271
  from: 2026-06-01T08:00:00Z
  to: null                    # null = 当前在位
```

- 设备履历查询 = 按 serial 串联其全部 occupancy 区间（跨位置连续）→ M2 备件、M4 故障预测的数据基础
- `cios apply -f pod003.yaml` 注册；全部资产 YAML 可入 Git（GitOps）

---

# 7. 关系模型

路径隐含 `contains`；其余关系用边声明，**边可带属性**（容量管理/冗余分析依赖）：

```yaml
kind: Relation
spec:
  type: feeds                  # feeds | cools | connects
  from: site01.pod003.busbar000.tou005
  to:   site01.pod003.rack005
  attributes:
    rated_kw: 30               # 额定容量 → M2 剩余容量计算
    redundancy_group: A        # A/B 路、N+1 组标记 → 冗余分析
```

- `feeds`：switchgear/breaker/tou → pod/rack/chiller（电力拓扑，断电影响面分析）
- `cools`：chiller → pod（冷却拓扑，chiller 故障波及哪些 pod；属性 `rated_cooling_kw`）
- `connects`：网络连接（属性 `bandwidth_gbps`）
- tou→rack 的 feeds 边在模板实例化时按序号对齐规则自动生成
- 告警根因分析、容量余量计算（M2）依赖这两张图，注册站点时必须录入

---

# 7bis. 端口（typed connection endpoints，v1.1，L101 item A）

**动机**：E3.6a Domain-C 需要设备声明液路连接端点，使 USD 侧可显式互联（承载形式 =
USD typed relationship，BP-10 已拍，细节归 spec-007）。端口是**语义层/数据层概念，非渲染概念**。

1. **端口在类型上声明**：`types.yaml`（及 ext.d 片段）的类型条目可选 `ports:` 键——列出该类型
   **可以拥有**的端口名。add-only，同 L54 纪律。
2. **首批端口词汇 = `liquid_in` / `liquid_out`**，注册于 types.yaml 顶层 `ports:` 词汇表。端口名
   进词汇互斥铁律（§1 铁律 6 扩展：端口名 ∩ 类型 ∩ 位置段 ∩ 量纲 = ∅）。
   **CI 注**：speccheck 的端口互斥检查为待扩项（基座 PRMT 批次落地）；v1.1 发布时人工核验无冲突。
3. **实例端口数量由型号模板（§4.5）给出**，同子资产 count 机制；模板未声明 = 无端口。
4. **关系边（§7）新增可选属性 `from_port` / `to_port`**：把 `cools`/`connects` 边钉到具体端口
   （如 chiller `liquid_out` → pod `liquid_in`）。不新增关系类型。
5. **不铸 power 端口**（评审 2/9）：电力可视化走既有 `feeds` 边；真实 BOM 需要时经 ext.d 增列。

```yaml
kind: Relation
spec:
  type: cools
  from: site01.chiller002
  from_port: liquid_out        # 可选 (v1.1): 钉到端口
  to:   site01.pod003
  to_port: liquid_in
  attributes:
    rated_cooling_kw: 800
```

---

# 8. 遥测集成

测点寻址、Query/Set 动词、量纲字典、供回水表达、Prometheus 映射见 **Spec 002 — Telemetry Points**。

---

# 9. 未决问题

| # | 问题 | 阻塞 |
|---|------|------|
| Q6 | 首批模板文件清单：AC40/AC45/DC40/DC45/HybridChiller 各型号的实际配置数据（tank/rack 数、node 数、GPU 数、组件清单）需产品侧提供 | M0 模板交付 |

已决：

- ~~Q4 site 代号~~ → 城市优先（citycode+seq），fleet-registry 注册分配（v0.5，D1）
- ~~Q3 network 清单~~ → fabricswitch(roce/ib)/coreswitch/router/firewall/oobswitch，
  带内带外一并建模，plane 属性三平面（v0.5，D2）
- ~~Cluster 跨站~~ → Cluster 纯虚拟、可跨站点组合，fleet 持有权威定义（v0.5，D5）
- ~~WUE 计量~~ → 新增 watermeter 类型（chiller/site 级），闭式系统仍需补水计量（v0.5，D4）

- ~~AC45 结构~~ → 产品族规则：AC 系列含 tank，DC 系列含 rack；具体结构由模板文件定义
- ~~Chiller 组件~~ → 由 chiller 模板文件定义，加载时装入
- ~~电表位置~~ → 位置由路径决定，归集按路径层级去重
- ~~域段去留~~ → 移除，域 = 顶层类型派生属性（v0.3）
- ~~busbar 下 rack 段~~ → tou 序号对齐所供 rack + feeds 边（v0.3）
- ~~路径身份语义 S1~~ → 双层身份：路径=位置（可换件复用），serial=物理实体+occupancy 区间（v0.4）
- ~~序号位数 S6~~ → 设备级三位补零，芯片级不补零对齐硬件（v0.4）
- ~~词汇冲突 S2~~ → 量纲 valve 改名 opening；词汇互斥进铁律与 CI（v0.4）
- ~~安防边界 B1~~ → 消防/门磁/烟感作测点接入；门禁/视频管理不做，第三方事件集成（v0.4）
- ~~Org 层级 D35~~ → 显式 Org 对象，tenant→Org→site；Cluster ⊂ Org；名租户内唯一（v1.1，L84/L99/L101 §5bis）
- ~~租户记录 D33(b)(d)~~ → tenants 记录 + `isolation_tier` 字段 + append-only 审计（v1.1，L98/L101 §5bis.1）
- ~~端口语义~~ → 类型级 `ports:` 声明 + `liquid_in`/`liquid_out` 首批词汇 + 关系边 `from_port`/`to_port`（v1.1，L101 §7bis）

---

# 10. Core / Scene Engine 计算边界（v1.1，L101 item C；交叉引 L80/L90/spec-009）

Core 计算**业务事实**且是其**唯一权威源（SoT）**——派生指标、告警、容量、margin 等一律出自
Core。Scene Engine 与各渲染端（WebGL / Omniverse）只**消费**业务事实，自身仅可计算**场景态**
（可见性、着色（L92）、动画、shader 参数）。在 Core 之外重算业务事实 = 不合规。
