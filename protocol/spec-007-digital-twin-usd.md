# CIOS Spec 007 — Digital Twin / USD 映射（Omniverse, ADR-7）

> 版本 **v0.5 DRAFT（未冻结；E3.6a Phase-1 合约见 §0bis；§11 Q1–Q8 **PASS 2026-07-13** [Q4→spec-009§4.1；Q5→§5bis]；编码解锁：usdmap PRMT-201 / Kit PRMT-202+；启动门 BP-2/10/12/15/23+L101 已清 2026-07-10）**｜架构者起草 2026-06-21 · L80 2026-06-22 · §5bis 2026-07-03 · **E3.6a kickoff + Q PASS 2026-07-13**
> 依赖（**只读消费，不修改**）：spec-001（资产模型/crn 路径树）、spec-002（遥测点位/量纲/单位）、spec-004（只读 API：`/v1/assets`、`/v1/metrics/query`）、spec-006（部署形态）。
> 上位决策：**ADR-7 / L45（L80 细化）**（Digital Twin = NVIDIA Omniverse，CIOS **不开发独立 3D 应用 / 不自研照片级 3D**，只交付 USD 映射 + 型号包 USD 资产 + Kit 数据连接扩展）；**L80**（体验层三层架构：spec-007 = USD 映射 + Omniverse 渲染器；运维 WebGL 渲染器与 Scene Engine 在 spec-009；二者共用场景）；**L49**（部署=云端，dev 用 Omniverse Standard 免费 license）。
> **里程碑归属：M3（L72，2026-06-21，backbone-first——E2.9 从 M2 移 M3）。** M3 一阶段 = **只读 live twin**（结构 + 遥测着色，仅查看）；交互控制（Query/Set 透传）= M3 后段/M4。实现门控 = Q1–Q9 + D11 + EXT；spec 维持 v0.1 DRAFT，不冻结。

---

# 0bis. E3.6a Phase-1 contract (2026-07-13 kickoff · P682 / P65x)

> **Status:** DRAFT binding for E3.6a implementation. **§11 Q1–Q8 PASS 2026-07-13** → coding PRMTs unlocked (PRMT-201 usdmap; PRMT-202 Kit skeleton).  
> **Authority:** M3-SCOPE §1bis; BP-2/10/12/15/23 locked 2026-07-10; Instruction `docs/architect-instruction-2026-07-13-e36a-omniverse-kickoff.md`.  
> **Not WebGL:** L90 distribution (portal `/noc/3d`, `tools/scene-engine`) is a **separate** surface. This contract is **USD → Omniverse**.

## 0bis.1 In scope (Phase 1 / M3 body)

| Domain | Phase-1 deliverable |
|--------|---------------------|
| **Domain-M** | Load model pack USD; show/hide/focus; **alarm overlay** from `/v1/alarms` only (default coloring + list; H5) |
| **Domain-C** | System ports `liquid_in` / `liquid_out` (spec-001 v1.1) carried as **USD typed relationships** (BP-10); optional simple flow viz when data ready |
| **Domain-S** | Weather/margin **display** + PhyX **visualization** fields only — **no** solve, **no** writeback, **no** closed loop (E3.6b) |

**MVP acceptance (BP-15 + BP-23):** run **DC45 or AC45** (either one); temperature display error ≤1℃; alarm overlay delay ≤10s; fluid animation delay ≤2s (when flow viz enabled).

## 0bis.2 Deploy configs (BP-2)

| Config | Role |
|--------|------|
| Full Omniverse | Engineering / high-fidelity validation |
| Kit lite | **Default development** (Composer + extension) |
| Streaming proxy | Thin remote client (later) |

## 0bis.3 CIOS deliverables vs non-deliverables

| Deliver (this repo / data) | Use (do not build) |
|----------------------------|--------------------|
| This spec + §5bis packs | Omniverse Composer / Kit app |
| Offline mapper (`tools/usdmap` or `cmd/cios-usd`) | Hydra/RTX render |
| Kit Python extension (poll CIOS, write USD attrs) | Nucleus prod service (after D11) |

**MUST NOT:** modify `core/` / `gateway/` for OV; USD→WebGL; invent absolute coordinates; multi-tenant OV distribution.

## 0bis.4 Live path (architect rec until Q2/Q3 lock)

1. Mapper builds site stage: prim tree §2 + model `references` §5 + optional layout sublayer §9bis.  
2. Kit extension **polls** `/v1/metrics/query` (+ assets/alarms as needed) on a **1–5 s** facility cadence (Q3 rec).  
3. Writes `cios:tlm:*` and consumes shared **visual_state** (spec-009 §4.1 / L92).  
4. Fail-soft: API down → keep last values + stale marker.

## 0bis.5 Implementation gate (unchanged spirit)

**Gate met 2026-07-13:** Q1 + Q2 + Q3 + Q6(D11 dev path) + Q7 + Q8 ratified (Instruction §9). Mapper = `tools/usdmap` (PRMT-201). Kit extension = `tools/ov-ext/` (PRMT-202+).

---

# 0. 本规范如何**不**给代码带来混乱（最高优先，必读）

spec-007 与 spec-001–006 的关系是**单向下游消费**，刻意设计为零侵入：

1. **不是权威源**：资产模型权威永远是 spec-001；遥测权威永远是 spec-002/VM。spec-007 只把它们**投影**到 USD，不新增、不改写任何 CIOS 数据/契约。
2. **零改动既有代码与规范**：实现 spec-007 **不得**修改 `core/`、`gateway/`、`pkg/`、`cmd/cios-*`（除新增独立产物外）、`store.go`/任何 Store 接口、spec-001–006、migrations。它**只通过既有只读 HTTP API + VM 查询**取数。
3. **独立进程/产物**：CIOS 侧交付物 = (a) 本映射规范，(b) 每型号一个 USD 资产包（数据文件，非 Go），(c) 一个**映射器**（CMDB→USD stage 生成器，独立二进制/工具，建议 `cmd/cios-usd` 或 `tools/usdmap`，**不进 core**），(d) 一个 **Omniverse Kit Python 扩展**（住在 Omniverse 侧，非本仓 Go 代码）。这些都不与 §M2 硬化批/任何接口耦合簇相关。
4. **只读**：M2 不回写 CIOS（Set 透传 = M3/M4，独立、需另行拍板）。
5. **独立线、门控**：实现 = E2.9，**不属于任何当前批次**；任何编码 PRMT 需 (i) 本 spec 升 v1.0（Q1–Q9 拍板）、(ii) D11（Nucleus/license）落定、(iii) 大概率 EXT（型号包几何/布局输入）。在此之前**不签发 Omniverse 编码 PRMT**。

> 一句话：spec-007 是"读 CIOS、写 USD"的旁路投影。它能造成的最坏情况是它**自己**坏掉，不会动到 site/告警/工单主链。

---

# 1. 范围 / 非目标（红线 = ADR-7 / L45，**L80 细化**）

**交付**：CIOS 路径树 ↔ USD prim 层级**同构映射**规范；型号包 USD 资产（注册即建景）；映射器；Kit 数据连接扩展（live 遥测着色 / 未来 Query-Set 透传）。
**非目标（红线）**：
- **不开发独立 3D 应用**；**照片级 3D 查看一律用 Omniverse 标准客户端**（USD Composer / Kit app）。CIOS 不自研照片级 3D 引擎。
- **照片级 3D 渲染归 Omniverse**（L45 内核不变）。~~Web 端不做任何 3D 渲染~~ **已由 L80 细化**：体验层新增**运维 WebGL 渲染器（示意/2.5D 空间 NOC）为获准 Web 表面**，与 Omniverse 同为 Scene Engine 的渲染器（spec-009 §2）。spec-007 仍仅负责 **USD 映射 + Omniverse 渲染器**，不含 WebGL 渲染器（其规范在 spec-009）。
- 不把 USD 作为任何 CIOS 数据的权威源或回写通道（M2）。

---

# 2. 路径 ↔ prim 同构（核心契约）

CIOS crn 路径树与 USD prim 层级**确定性、可逆**对应：

```
crn:  sgp01.pod002.cdu000.fws.supply.flow      （spec-001 §2）
prim: /World/sgp01/pod002/cdu000/fws/supply/flow
```

- 规则：crn 段按 `.` 切分，逐段成为 prim path 段，根挂 `/World`。段字符已受 spec-001 `^[a-z0-9.]+$` 约束 → 天然是合法 USD prim 名（USD 允许 `[A-Za-z0-9_]`；CIOS 段无大写/无特殊符，安全；**点不会进入单个 prim 名**，因为点是层级分隔符）。
- 可逆：prim path 去掉 `/World/` 前缀、`/`→`.` 即还原 crn。映射器与 Kit 扩展共用此唯一函数（禁止两处各写一份）。
- 资产 prim = `Xform`（可挂变换 + 子 prim）；遥测点位（叶）= 资产 prim 上的**属性**（§4），不单独成 prim（避免 prim 爆炸；2.75 万点/站若每点一 prim 不可接受）。

---

# 3. 类型 → prim 结构 + 属性命名空间

- `Asset.Type`（spec-001 / types.yaml）→ USD prim 的 `kind` + 引用的型号包资产（§5）。**不**为每类型造重型自定义 USD schema（C++ schema 维护成本高）；M2 用 **`Xform` + `cios:` 命名空间自定义属性**承载元数据。
- 属性命名空间（避免与 USD/Omniverse 原生属性冲突）：
  - `cios:type`（资产类型）、`cios:path`（crn，回链）、`cios:lifecycle`（spec-008 §13.1）。
  - 遥测：`cios:tlm:<quantity>`（如 `cios:tlm:power_watt`、`cios:tlm:temp`），值由 Kit 扩展 live 刷新（§6）。
- 单位见 §4；着色见 §6。

---

# 4. 单位制

- USD stage `metersPerUnit` / `upAxis` = D11 期定（建议 metersPerUnit=1.0 即米、upAxis=Z）。
- 遥测属性的物理单位**沿用 spec-002 units.yaml**（如 watt/celsius/lpm）；在属性上以 `cios:unit:<quantity>` metadata 标注，**不做单位换算**（保持 unit-pure，与 spec-002 一致）。
- 几何尺寸单位由型号包资产自带（§5），与遥测单位正交。

---

# 5. 型号包 USD 资产（注册即建景）

- 每资产**型号**提供一个参考 USD 资产（`.usd`/`.usdz`，含几何 + 默认材质 + `cios:` 属性骨架 + **相对**变换）。
- 注册一个 CIOS 资产（CMDB 新增）→ 映射器在对应 prim path **引用**（USD `references`）该型号资产并实例化 → "注册即自动装配场景"。
- 型号包是**数据产物**（独立版本化，建议 `assets/usd/<type>/...` 或独立仓），不是 Go 代码；缺失型号包的资产降级为占位 `Xform`（不阻塞）。
- **来源管线（Yuri 2026-06-12，TODO T26）**：工程侧既有 `.blend` 模型（如 `DC45_Flow.blend`）→ **Blender 直接导出 USD**，零额外建模管线。导出约定 = **§5bis（2026-07-03 并入，D43）**；故型号包来源已通，无需自研建模工具（再次印证"不开发 3D 应用"红线）。
- 存放/版本化（原 Q5）**已决（Yuri 2026-07-03，D43）**：`assets/usd/<type>/` 仓内、plain git（无 LFS；接受二进制历史增长，纪律 = 仅几何/约定实变更才重导出）。

---

# 5bis. 型号包 USD 导出约定（Model-Pack Export Convention，T26 / D43）

> Scope: **every 型号包 USD asset delivered to CIOS** — internal exports (Blender) and external vendor deliveries (chiller, BESS, switchgear, transformer, genset suppliers). Vendor-facing derivative: `vendor-usd-model-delivery-guide` (this section is the single source of truth). Decisions Q-B1–Q-B5 拍板 2026-07-03（Yuri 授权架构者，D43）。

## 5bis.1 Deliverable definition

- One 型号 = **one self-contained `.usdc` file** (USD binary crate). No external dependencies: no sublayers, no external references, no on-disk textures (embedded or omitted).
- Filename = `<MODEL>.usdc`, `<MODEL>` = exact spec-001 `model` attribute value.
- Stage `customLayerData` (MUST): `cios:modelpack:model` (= filename stem) · `cios:modelpack:version` (semver, bump every re-export) · `cios:modelpack:source` (tool+version) · `cios:modelpack:date` (ISO date).
- Committed location: `assets/usd/<type>/<MODEL>.usdc` (§5 存放决定)。

## 5bis.2 Stage requirements (MUST)

- `upAxis = Z`, `metersPerUnit = 1.0` (true scale, meters), `defaultPrim = /root`; `/root` = `Xform` with **identity transform**.
- Origin/orientation **[Q-B1 DECIDED]**: origin = center of unit footprint at ground plane (geometry rests on z ≥ 0); **+X = unit front** (primary service/door side). Site-draw (§9bis) and the mapper place instances assuming this.

## 5bis.3 Prim hierarchy (MUST)

```
/root                      (Xform, defaultPrim; cios:type, cios:model)
├── geo/                   all visual geometry
│   ├── <type><index>/     bindable component, protocol vocabulary (5bis.4)
│   └── frame/             non-mapped dressing: enclosure, doors, cables, labels
├── sen/                   sensor markers (5bis.6); may be empty
└── _materials             (Scope)
```

- No geometry prims directly under `/root` (no flat object dumps).
- Bindable nesting mirrors spec-001 parentage (DC45 pod pack: `/root/geo/rack000/rdhx000`; chiller pack: `/root` itself is the chiller → `/root/geo/drycooler000/fan002`).

## 5bis.4 Naming (MUST)

- All prims: `[a-z0-9_]+`; `_` only in non-bindable dressing (`frame/`, `_materials`).
- Bindable components: `<type><index>`, `<type>` ∈ types.yaml ∪ ext.d (closed set, L64), `<index>` = 3-digit zero-padded per spec-001 §2 template numbering (chip-level natural, e.g. `gpu0`).
- MUST NOT: `.001`/`_001` duplicate suffixes, uppercase, vendor product names as prim names (`CRAH3` → `ceilair003`), spaces, non-ASCII.
- Missing vocabulary → L54 ext.d fragment process; exporters/vendors **never invent type names**.

## 5bis.5 `cios:` attribute skeleton (MUST)

- `/root`: `cios:type` + `cios:model`. Each bindable component: `cios:type`. Each sensor marker: `cios:relpath` **[Q-B2 DECIDED: new attr; `cios:path` stays absolute-crn, mapper-owned]**.
- `cios:tlm:*` / `cios:unit:*` (§3) are runtime, mapper/Kit-owned — packs MUST NOT pre-bake values. No other `cios:*` may be invented by exporters.

## 5bis.6 Sensor markers (`sen/`)

- Marker = locator prim at physical sensor location, named per 5bis.4; carries `cios:relpath` = crn suffix relative to unit root (e.g. `sensor003`, `rack000.rdhx000`); point-level suffixes = spec-002 vocabulary only.
- Markers ship **in the single pack** **[Q-B3 DECIDED]** — no `_Monitor` variant file; renderers toggle `sen/` visibility.

## 5bis.7 Prohibitions (MUST NOT, lint-enforced)

Lights (any UsdLux — instanced references multiply lights) · cameras · animation/timeSamples/rigs/physics/audio · scene context (rooms, floors, neighbouring units; the unit's own enclosure = `geo/frame/`) · external file deps · any transform on `/root` · pre-baked `cios:tlm:*` values.

## 5bis.8 Budgets (SHOULD, warn-level **[Q-B4 DECIDED]**; waiver reason recorded in intake EXT/D line)

file ≤ 50 MB · triangles ≤ 500 k · materials ≤ 64 · textures ≤ 2048² · prims ≤ 5 000.

## 5bis.9 Validation gate

Automated lint per delivery: stage (5bis.1–.2) / hierarchy (5bis.3) / naming incl. vocabulary closure (5bis.4) / skeleton (5bis.5–.6) / prohibitions (5bis.7) / budgets (5bis.8, warn). Tool = **`tools/usdlint`** **[Q-B5 DECIDED]**, independent tool per §0.3; PRMT only after this section is merged (done 2026-07-03); `目标分支: feature/m3-model` (L94). Until the tool exists, the gate runs manually with the same checklist. Visual review (scale/origin/orientation) always included.

## 5bis.10 Vendor delivery process

Vendor gets the delivery guide + relevant vocabulary excerpt → delivers `<MODEL>.usdc` + component manifest (prims → physical parts, markers → datasheet measuring points) → intake registered as **EXT-NNN** → 5bis.9 gate → accepted pack lands in `assets/usd/<type>/` with a D-entry line (model, version, EXT source). Missing vocabulary raised in EXT review (L54), never invented by vendor.

## 5bis.11 Blender export profile (informative)

Native USD export, `.usdc`, selection-only on model collection; meters, Z-up, USD Preview Surface, no lights/cameras/animation, apply modifiers + object scale; pre-export cleanup: rename per 5bis.4, group into `geo/<component>` + `geo/frame` + `sen`, verify origin per 5bis.2.

## 5bis.12 Migration of 2026-07-03 intake (4 files, `feature/m3-model`)

| file | action |
|---|---|
| AC45.usdc | re-export per 5bis.2–.7: group flat prims; `QM9700_*` → `switch00N`; `EATON_93Li` → `ups000`; labels/cables/floor → `frame/`; strip DomeLight |
| DC45_STD.usdc | merge with Monitor; `CRAH*` → `ceilair00N`; `DLC*` → `rack00N`; `RDHx*` → `rdhx` under owning rack; `PICV/Valve*` → `valve00N`; strip lights |
| DC45_Monitor.usdc | 255 `MON_*` markers → `sen/` with `cios:relpath` in single DC45 pack; `_001` suffixes eliminated; file retired |
| DC45_Gemini.usdc | demo asset, out of scope (Gemini = solution 非型号, Yuri 2026-07-03/D43); not committed to `assets/usd/` |

---

# 6. Live 遥测叠加 + 着色（只读）

- **传输（Q2）**：M2 = Kit 扩展**轮询** spec-004 只读 API（`/v1/metrics/query` 或 `/v1/assets` + VM），按节流频率刷新 `cios:tlm:*` 属性。push（NATS 桥）/ USD live 层（Nucleus）= 后续。
- **着色约定（Q4 → 收口于 spec-009 §4.1 / D41）**：状态/温度 → 视觉（如温度→displayColor 渐变、告警态→emissive）。**色带/阈值的单一权威已上提到 spec-009 §4.1（D41）**——渲染器无关的语义映射，Omniverse 与 WebGL 共用，引用既有 quantities.yaml/units.yaml + spec-003 severity，不新增量纲。本 spec 的 Omniverse 渲染器消费该共享约定，不自带局部定义。
- **只读**：扩展只**读** CIOS、**写** USD 属性；绝不回写 CIOS（M2）。
- **失败软**：API/VM 不可达 → 属性保持上次值 + 标记 stale，不崩 stage。

---

# 7. CIOS 交付物 vs 使用物

| CIOS 交付（本仓/数据产物）| 使用（不开发）|
|---|---|
| 本映射规范（spec-007）| Omniverse 标准客户端（USD Composer / Kit app）查看 |
| 型号包 USD 资产（§5）| Nucleus / 场景服务（部署 D11）|
| 映射器（CMDB→USD stage，独立工具）| USD/Hydra 渲染（NVIDIA 提供）|
| Kit 数据连接扩展（live 着色；未来 Set 透传）| |

---

# 8. 分期

- **M3 一阶段（本 spec 范围）= 只读 live twin**：结构同构 + 型号包装配 + live 遥测着色，仅查看。（原拟 M2，2026-06-21 L72 移 M3，backbone-first。）
- **M3/M4 = 交互控制**：Omniverse 内操作 → Query/Set 透传回 CIOS（经 spec-002 set 动词 + 审计 + 双人审批，spec-002/spec-006 §5）。**本 spec 仅占位，控制契约另行起草，需安全评审。**

---

# 9. 物理布局缺口（重要，勿臆造）

CIOS 资产模型（spec-001）是**逻辑层级**，不含 3D 坐标。prim **层级**同构由 §2 保证，但 prim 的**空间摆放**（translate/rotate/scale 的绝对值）CIOS 当前无数据源：
- 型号包资产自带**相对/局部**几何（§5）。
- **站点/机房/机架的绝对布局**（rack 坐标、行列间距、朝向）= 缺失输入，需布局数据源（设施 CAD / 人工布局文件 / EXT）。**spec-007 不发明坐标**；M2 可先用确定性自动布局（按路径树规则网格摆放）占位，真实布局 = 后续输入（记 D / EXT）。**Site-Draw（§9bis，L79）= 此布局缺口的人工编排解**：由 Site Admin 在 Omniverse 内摆放/连接型号包，产出权威布局层，取代自动网格占位。

---

# 9bis. Site-Draw — 站点编排（M3 / E3.6，L79）

**目的**：让 Site Admin/investor 通过**复制 + 连接 + 摆放**型号包，建起一个 site 的完整 digital twin，并填补 §9 的绝对布局缺口。**这是 §9「真实布局后续输入」的人工编排实现，不是新增的独立 3D 应用**（不破 L45/ADR-7：仍在 Omniverse 标准客户端 / Kit 扩展内完成）。

**读写边界（L79，红线）**：
- **型号包 USD 资产本身只读/不可变**（§5 数据产物，版本化分发）。site-draw **引用（references）**它，不修改其几何/材质源。
- **实例级属性可编辑**：每个被放置实例的 transform（translate/rotate/scale，即 §9 缺的绝对布局）、连接关系（管路/电气/网络拓扑边）、命名、**绑定的 CIOS 路径**、以及实例局部 `cios:` 属性覆盖——经 USD reference + 局部 override 层表达。
- **写 USD layout 层，不回写 CIOS core**：CMDB（spec-001）仍是资产权威源；site-draw 产出的是 USD 场景/布局层（建议独立 layout sublayer，可版本化、可重生）。twin → CMDB 回写（"画即建模"）= **另行拍板，不属本条**（破 §0 单向红线，需新写路径 + spec-001/007 变更）。

**与既有节的关系**：
- §5 注册即建景 = **数据驱动自动装配**（CMDB→prim references）；site-draw = **人工编排叠加**（布局/连接/克隆）。二者同写 USD、互补：自动装配保证层级同构（§2），site-draw 补空间布局与拓扑连接。
- §6 live 着色（只读遥测）作用于 site-draw 摆好的实例上——结构（draw）+ 数据（live）分层。

**门控**：受 spec-007 Q1（schema）/Q2（传输）/Q7（布局源）/Q6=D11（部署）约束；落地排在 §8 只读 live twin 一阶段之后或并行，独立线，不挤占 backbone。

---

# 10. 部署（= D11，待定）

- 形态：云端（L49）。Nucleus/场景服务位置、license（dev=Omniverse Standard 免费）、网络（OT/IT 隔离对 Omniverse 拉流的影响）= **D11 收口后**才进实现。

---

# 11. 未决问题（评审拍板 → 升 v1.0 + L号；实现门控）

| # | 问题 | 架构者倾向 | 状态 |
|---|------|-----------|------|
| Q1 | USD schema 方式 | `Xform`+`cios:` 自定义属性（轻），不做 C++ API schema | DISCUSSION |
| Q2 | live 遥测传输 | M2 轮询只读 API（节流）；push/live-layer 后续 | DISCUSSION |
| Q3 | 刷新节流频率 | 按设施侧采样（~1–5s 显示足够），与 L52 GPU 高频解耦 | DISCUSSION |
| Q4 | 着色/材质约定 | 温度渐变 + 告警 emissive | **已收口 → spec-009 §4.1（D41 → L92 锁定 2026-06-28）**：单一权威、双渲染器共用；Omniverse = USD material override 消费 visual_state，不自带局部定义 |
| Q5 | 型号包来源/版本化/存放 | 独立版本化数据产物；缺包降级占位 | **已收口 → §5bis（D43，2026-07-03）**：来源 = Blender/vendor 按 §5bis 导出约定；存放 = `assets/usd/<type>/` 仓内 plain git；版本化 = customLayerData semver。缺包降级占位不变 |
| Q6 | Nucleus/场景服务部署 + license | = **D11** | 待 D11 |
| Q7 | 物理布局坐标源（§9）| 型号包带局部几何；站点绝对布局 = 外部输入，M2 自动网格占位 | DISCUSSION（可能 EXT/新 D）|
| Q8 | 映射器形态 | 独立工具（`cmd/cios-usd`/`tools/usdmap`），不进 core；批量生成 + 增量更新 | DISCUSSION |
| Q9 | M3/M4 控制透传契约 | 复用 spec-002 set + 审计 + 双人审批；另行起草 | 占位（M3/M4）|
| Q10 | **Site-Draw 布局层持久化/格式**（§9bis，L79）| 人工编排产物存为独立 USD layout sublayer（可版本化、与自动装配层叠加、可重生）；连接拓扑用 USD relationship。与 Q7 布局源协同：site-draw = 人工布局源 | DISCUSSION（L79 定方向，细节随实现）|

> **实现门控**：Q1–Q9（至少 Q1/Q2/Q6/Q7）拍板 + D11 落定前，**不签发 Omniverse 编码 PRMT**。E2.9 在 M2 退出主链（§M2-1..4）之后或并行独立推进，**不挤占硬化/收口**。

---

# 12. CHANGELOG

| 版本 | 日期 | 变更 |
|------|------|------|
| v0.1 DRAFT | 2026-06-21 | 首版草稿：形式化 ADR-7/L45/L49 + E2.9 范围为 spec-007——§0 解耦不混乱保证、§2 路径↔prim 同构、§3 类型/属性命名空间、§4 单位、§5 型号包、§6 live 着色、§8 分期、§9 物理布局缺口、Q1–Q9。**未冻结、无 L 号**；实现门控（Q 拍板 + D11）。与 spec-001–006 零改动、只读下游。|
| v0.2 DRAFT | 2026-06-21 | 新增 **§9bis Site-Draw 站点编排（L79）**：人工复制/连接/摆放型号包建 site twin，填 §9 布局缺口；**型号包只读、实例属性可编辑、写 USD 不回写 CIOS core** 红线明确；新增 Q10（布局层持久化）。归 M3 / E3.6。仍未冻结、门控不变。|
| v0.3 DRAFT | 2026-06-22 | **L80 细化交叉更新**：§1 红线「Web 端不做任何 3D 渲染」按 L80 细化为「照片级 3D 归 Omniverse；运维 WebGL 渲染器（spec-009）获准」——spec-007 仅负责 USD 映射 + Omniverse 渲染器，WebGL 渲染器 + Scene Engine 在 spec-009。**Q4 着色约定收口至 spec-009 §4.1（D41）**：单一权威、双渲染器共用。上位决策加 L80 指针。仍未冻结、门控不变。|
| v0.4 DRAFT | 2026-07-03 | **§5bis 型号包导出约定并入（T26 交付，D43）**：deliverable/stage/hierarchy/naming/`cios:` skeleton/sensor markers/prohibitions/budgets/lint gate/vendor 流程/Blender profile/4 文件迁移表。Q-B1–Q-B5 拍板（Yuri 授权架构者 2026-07-03）：footprint-center 原点 +X front、新 `cios:relpath`、单包 `sen/`（`_Monitor` 变体退役）、预算 warn 级、`tools/usdlint`（PRMT 后置，`feature/m3-model`）。**Q5 收口 → §5bis**（存放 = `assets/usd/<type>/` plain git，Yuri 2026-07-03）。同批决定：Gemini = solution 非型号（无 spec-001 enum 变更）。仍未冻结、其余门控不变。起草与决策记录：`docs/tmp/spec-007-5bis-usd-export-convention-draft-2026-07-03.md` + `docs/tmp/model-usd-intake-2026-07-03.md`。|
| v0.5 DRAFT | 2026-07-13 | **E3.6a kickoff / P682**：新增 **§0bis E3.6a Phase-1 contract**（Domain-M/C/S 范围、BP-2/10/15/23 绑定、mapper+Kit 边界、与 WebGL/L90 分界、live poll 建议、编码门 = Q1/Q2/Q6/Q7）。对应 Instruction `docs/architect-instruction-2026-07-13-e36a-omniverse-kickoff.md`。**不**单独升 v1.0、**不**自动解锁 Kit 编码 PRMT。|
