# CIOS Spec 001 — 附录 A：类型属性 Schema（T11）

> 版本 **v1.1（2026-07-10，随本体 L101 升版：A.2 新增 `city` 可选属性，其余不变）**｜归属：spec-001 v1.1 附录
> v1.0（冻结，2026-06-13 评审会）
> 评审批注（Yuri）：**计费暂不考虑——tariff_ref/grid_operator 等计费相关字段/属性/接口仅作预留**（M3 启用），不参与 M0–M2 校验
> 原则：属性 = 不随时间变化的**铭牌/配置事实**；会变的量是遥测（spec-002），不进属性。

## A.1 通用属性（所有 device 级类型）

| 属性 | 类型 | 必填 | 说明 |
|------|------|------|------|
| serial | string | ✅（physical 实体） | 物理实体身份，CMDB occupancy 锚点（双层身份之"物"） |
| model | string | 设备类型而定 | 厂家型号；有型号包的类型必填 |
| template | string | 模板实例化类型必填 | 型号包引用（pod/chiller 等） |
| vendor | string | 可选 | 厂家名 |
| commissioned_at | date | 可选 | 投运日期（维保周期计算用，M2） |
| label | string | 可选 | 人读别名，不参与寻址 |

## A.2 site 属性（**计费/告警硬依赖**）

| 属性 | 类型 | 必填 | 说明 |
|------|------|------|------|
| timezone | IANA tz string | ✅ | **计费账期、TOU 电价段、报表口径的唯一时区权威**；UTC 存储 + site tz 呈现 |
| region | string | ✅ | fleet-registry 分配口径（site 代号前缀来源，D1）——**v1.1 语义不变** |
| city | string | 可选（v1.1，L101） | 天气模板 key（E3.6a Domain-S）；自由文本城市名，歧义由 coordinates 消解 |
| grid_operator | string | 可选 | 电网/售电主体（计费对账） |
| tariff_ref | string | 可选 | 电价表引用（M3 计费启用，M0 留位） |
| coordinates | [lat, lon] | 可选 | 地图/气象关联（M4 预测用） |
| commissioning_state | enum: planned/commissioning/active/retired | ✅ | 站点生命周期；非 active 站点告警默认抑制（maintenance 语义对齐 spec-003） |

## A.3 各类型必填属性（与 types.yaml attrs_required 同步，机器可读以 YAML 为权威）

| 类型 | attrs_required | 备注 |
|------|----------------|------|
| pod | model, serial, template, rated_power_kw | rated_power_kw 用于容量/超售告警 |
| chiller | model, serial, template | L46 挂级规则见正文 §4.2 |
| node | serial | model 经 BMC 自动发现回填（M1） |
| gpu | serial | 序号对齐 DCGM index |
| fabricswitch | fabric (roce\|infiniband) | D2 三平面 |
| 其余 device 级 | serial | 通用最低要求 |
| chip 级（gpu 除外） | —（随父实体） | 不独立登记 serial |

> 变更流程：attrs_required 改动 = types.yaml PR（CI 校验）+ 本附录同步行；二者不一致以 YAML 为准并视为文档 bug。

## A.4 校验规则（cios apply / API 入口强制）

1. attrs_required 缺失 → 400 `bad-request`（detail 列缺失项）
2. timezone 必须是合法 IANA 名（Go time.LoadLocation 可加载）
3. serial 站内唯一（跨类型也唯一）；冲突 → 409 `conflict`
4. spec.type 必须 == 路径末节点类型（PRMT-011 已实现）
5. 未注册属性键：M0 透传不校验（spec 自由区）；M1 起未知键 warning

## A.5 未决（评审会定）

- node 的 model 是否升必填（自动发现到位前的过渡期口径）
- rated_power_kw 放 pod 还是下沉 rack/tank（影响容量告警粒度）
