# CIOS Spec 009 — Experience Layer / NOC + Scene Engine（B/S 数字孪生界面）

> 版本 **v0.3 DRAFT（未冻结；D37–D41 + D31 已拍板并折入；D39 SSE↔NATS 传输契约 §6.1 已锁 2026-06-26；余 Q9 控制透传 = M3 后段/M4 占位；上位 L80 已锁、**L81[D31] 已锁 2026-06-23**）**｜架构者起草 2026-06-22，D37–D41 + D31 折入 2026-06-22，§6.1 折入 2026-06-26
> 触发：Yuri 2026-06-22 提出 9 项 B/S 用户交互界面需求 + 拍板三层渲染架构（CIOS Core / Scene Engine / Renderer{WebGL, Omniverse}）。
> 依赖（**只读消费，不修改**）：spec-001（资产模型 / crn 路径树 §2 + 关系图 §7）、spec-002（遥测点位 / 量纲 / 单位）、spec-004（只读 API `/v1/assets`、`/v1/metrics/query`、`/v1/alarms`）、spec-006（部署形态 / mTLS / 审计 §5）、spec-007（USD 映射 + Omniverse 渲染器 + Site-Draw 布局 §9/§9bis）。
> 上位决策：**L80（2026-06-22 锁定，= D37）**——体验层三层架构 + L45 细化（照片级 3D 归 Omniverse；运维 WebGL 渲染器获准）。子决策 **D38–D41 已拍板（Yuri 2026-06-22 confirm）**，折入 §3/§4/§6。spec 仍维持 DRAFT（D31 鉴权 + Q9 控制透传未定）；**Phase A（M3.5）编码门控已足（见 §8），Phase B（closeout）门控 L80+D38–D41 已满足、待 D31**。
> 里程碑归属：**M3**，分两阶段（§8，Yuri 2026-06-22）：**Phase A = M3.5 轻量功能/数据验证页**（薄 `/v1` 消费者，验证接口+数据+所有功能点，无空间渲染）；**Phase B = M3 closeout 渲染**（Scene Engine + WebGL 空间渲染器 + Omniverse）。WebGL 运维 NOC = E3.5 升格（薄前端 → 空间运维台）；Omniverse = E3.6（spec-007）。二者共用 §3 Scene Engine。

---

# 0. 本规范如何**不**给代码带来混乱（最高优先，必读）

spec-009 与 spec-001–006 的关系是**单向下游消费**，与 spec-007 完全一致的零侵入设计：

1. **不是权威源**：资产/拓扑权威永远是 spec-001；遥测权威永远是 spec-002/VM；告警权威永远是 spec-003。本规范只把它们**投影**到场景与界面，不新增、不改写任何 CIOS 数据/契约。
2. **零改动既有代码与规范**：实现 spec-009 **不得**修改 `core/`、`gateway/`、`pkg/`、`cmd/cios-*`（除新增独立产物外）、`store.go`/任何 Store 接口、spec-001–006/008、migrations。它**只通过既有只读 HTTP API + VM 查询 + 遥测增量流**取数。
3. **独立进程/产物**：CIOS 侧交付物 = (a) 本架构规范，(b) **Scene Engine**（独立服务，从 CMDB+布局生成渲染器无关的场景描述，建议 `cmd/cios-scene` 或独立服务，**不进 core**），(c) **WebGL 运维 NOC 前端**（独立 SPA/SSR-Hybrid，TS/React，D30），(d) Omniverse 渲染器扩展（= spec-007，住 Omniverse 侧）。
4. **只读**：M3 一阶段不回写 CIOS（Set 透传 = M3 后段/M4，独立、需安全评审，见 §8 + spec-007 Q9）。
5. **独立线、分阶段门控**（见 §8）：
   - **Phase A（M3.5 轻量功能页）** 编码 PRMT 需 D30 + D31（§7.1）+ 既有 `/v1` RBAC——**全部满足 ✅，可签发**。**不需 D37/D38/D39/D41**。
   - **Phase B（M3 closeout 渲染）** 编码 PRMT 需 L80（已锁）+ D38–D41（已决）+ D31（已决）——**全部满足 ✅**，待 Phase A 验证后启动。

> 一句话：spec-009 是"读 CIOS、画界面"的旁路投影。最坏情况是界面**自己**坏掉，不会动到 site/告警/工单主链。

---

# 1. 设计目标（Yuri 9 项需求形式化）

| # | 需求 | 落点 |
|---|------|------|
| R1 | **B/S 架构**；交互逻辑 **全局 → Focus → 细节 → 数据细节局部刷新**（非全页刷新）| §3 Scene Engine + §6 数据流 |
| R2 | 登陆后展示整个 site 数字孪生；**仅渲染登陆用户可见资产**，越权资产以**透明轮廓**呈现（仅保留轮廓，细节不渲染）| §3.3 RBAC 场景过滤（服务端强制） |
| R3 | **异常高亮 → 点击查看原因 → 进入分析与操作**（SpaceX Starlink NOC 风格）| §5.2 异常下钻 |
| R4 | 对象下方显示关键数据（运行时间 / Power / State / Utilization 等）| §5.3 对象数据标签 |
| R5 | 页面右下角显示 Facility Power / IT Power / PUE 等**站点信息曲线**| §5.4 站点信息 chart |
| R6 | 多站点 ORG：左上角**透明下拉框**切换 site；该站点有异常则下拉项背景标注醒目提醒色 | §5.5 站点切换器 |
| R7 | **Topology + Flow 动画**（电流 / 冷却流）| §4.3 流动画（绑定 spec-001 §7 边 × 遥测） |
| R8 | 点击/悬停对象查看状态：**ID / 名称 / Status / 进出口 Temp{press, flow} / Alarm / action**| §5.6 对象检视器 |
| R9 | 保留**视图切换**（GPU view）；**M3 不启用**| §8 分期（对齐 D29 / L68 GPU/IT 遥测后置） |

---

# 2. 三层架构（核心红线 = L80，已锁 2026-06-22 = D37）

Yuri 2026-06-22 拍板的渲染分层。**一份场景，多个渲染器**：

```
┌─────────────────────────────────────────────────────────┐
│  Layer 3 — Renderer                                       │
│    • WebGL 运维 NOC（浏览器原生 3D/2.5D，B/S，本规范主体）   │
│    • Omniverse（照片级 3D，Kit 客户端 / 流式，= spec-007）   │
├─────────────────────────────────────────────────────────┤
│  Layer 2 — Scene Engine（USD-like，渲染器无关场景）         │
│    几何 + 布局 + 拓扑边 + 遥测绑定 + RBAC 过滤              │
├─────────────────────────────────────────────────────────┤
│  Layer 1 — CIOS Core（权威，只读）                         │
│    Telemetry（spec-002/VM） + CMDB（spec-001 §2 路径树）    │
│    + Graph（spec-001 §7 关系图 feeds/cools/connects）       │
└─────────────────────────────────────────────────────────┘
```

**L45 细化（L80，已锁 2026-06-22 = D37）**：
原 L45/ADR-7「Web 端不做 3D 渲染，3D 一律 Omniverse」**细化**为：
- **照片级 3D 渲染仍归 Omniverse**（spec-007 不变）；CIOS **不自研照片级 3D 引擎 / 不复制 Omniverse 的渲染能力**。
- 新增**运维 WebGL 渲染器**（示意/2.5D 空间 NOC）为**获准的 Web 表面**：它消费 §3 Scene Engine 的渲染器无关场景，服务于"全局 → focus → 细节"的日常运维（R1），**不追求照片级真实感**。
- 二者是**同一 Scene Engine 的两个渲染器**，互不替代：WebGL = 随处可达的运维台；Omniverse = 沉浸式 / 投资人 / Site-Draw 编排。

> **为何需要新 L**：L45 字面禁止"Web 端 3D 渲染"，而 WebGL 渲染器**就是**浏览器侧 3D 渲染。本条不是 doc 澄清，是 L45 的实质演进 → 已经 D37 拍板沉淀为 **L80（2026-06-22 锁）**，spec-007 §1 红线 + 上位决策 + Q4 已同步交叉更新（spec-007 v0.3）。

---

# 3. Scene Engine（Layer 2，渲染器无关）

Scene Engine 是新增的独立只读服务，把 Layer 1 的权威数据组装成**渲染器无关的场景描述**，喂给 WebGL 与 Omniverse 两个渲染器。

## 3.1 场景来源（皆 spec-001/002 既有数据，零发明）
- **层级结构**：crn 路径树（spec-001 §2）→ 场景节点树。与 spec-007 §2 路径↔prim 同构**共用同一映射函数**（禁止两处各写一份）。
- **拓扑边**：spec-001 §7 关系图 `feeds`（电力）/ `cools`（冷却）/ `connects`（网络），含属性 `rated_kw` / `rated_cooling_kw` / `bandwidth_gbps` / `redundancy_group`。这是 R7 流动画的边来源——**已存在，不新建**。
- **绝对布局**：spec-007 §9 缺口 + §9bis Site-Draw 产出的 USD layout 层。WebGL 与 Omniverse **共用同一布局源**（D38 定 WebGL 如何取用 USD 布局）。布局缺失时降级为确定性自动网格占位（同 spec-007 §9）。
- **遥测绑定**：spec-002 点位 → 场景节点属性（沿用 spec-007 §6 `cios:tlm:<quantity>` 命名空间），值由 §6 增量流刷新。
- **告警绑定**：spec-003 告警态 → 节点 `cios:alarm` 状态，驱动 R3 异常高亮。

## 3.2 场景格式（D38 → **L90 锁定 2026-06-28**：USD 为源，单向转码 web 原生；四层职责 + 三红线见 L90）
- **USD = 唯一被编排源**（含 Site-Draw §9bis 布局）；**Scene Engine 单向转码**出 web 原生场景：**几何 = glTF / 3D-tiles**，**拓扑/遥测绑定/RBAC = 叠加 JSON**。Omniverse 继续读原生 USD。
- glTF/JSON 是**只读派生产物**（可重生），**非双权威**——守 §0 单向红线，无同步漂移。
- WebGL 取几何 = 按需 / 分块瓦片（3D-tiles）HTTP 拉取，CDN/缓存友好（与 §6 D39 配对）。
- 不变量：§3.1 来源与 §4 渲染器契约保持渲染器无关。
- 依据：`docs/D38-D41-render-decisions.md` D38 选项 B。

## 3.3 RBAC 场景过滤（R2，**服务端强制，安全红线**；D40 → **L91 锁定 2026-06-28**）
- **强制点 = USD graph level（L91）**：RBAC = USD graph-level security compiler，非渲染端过滤规则。服务端在「RBAC → 渲染器分叉」**上游**设独立 **USD pruning 层**（仅服务端），产出按身份的 **Authorized (policy-reduced) USD**（prim visibility pruning / stage composition exclusion / variant stripping / metadata redaction，具体机制实现期定 + 与 spec-007 协同）。master USD 仍 SSOT（L90）；authorized USD = 按身份只读投影。**pruning 在 Scene Engine 之前/之上——Scene Engine 永不评估权限**（守 L90 transcode-only；否则即「Scene Engine 误变权限层」风险）。
- 越权资产（不在登陆者 RBAC 路径-glob scope，L34/L50 语义）**仅下发轮廓几何**：**不**下发遥测值、对象明细、告警细节。
- **必须服务端裁剪后再下发**——**禁止**下发完整数据后由前端 dim（会泄露他人遥测）。这是 §0.4 只读之外的强约束。**轻量页（Phase A）经既有 `/v1` RBAC 已强制数据级过滤；几何级轮廓降级 = Phase B。**
- **two-stream（L91）**：Stream A（authorized）→ full geometry + telemetry；Stream B（forbidden）→ ghost/silhouette proxy only，**零遥测**，**禁** click event / metadata fetch / time-series binding。WebGL 表现 = wireframe/silhouette/low-alpha；Omniverse 表现 = ghost material variant、no physics/data channel。
- **fail-closed（L91 安全不变量）**：RBAC engine 不可用 / policy 解析失败 → **deny（绝不回退全量场景）**；不被 debug/perf/任何 shortcut bypass。粒度（整场景 empty vs per-asset deny + 外壳）= 实现期细化。
- **轮廓可见 + 敏感租户整体隐藏开关**：默认显示越权资产的透明轮廓（满足 R2）；多租户敏感场景下提供**按部署/租户的隐藏策略开关**（整体不可见），与 D33 隔离深度 / D35 Org 层级协同。
- **按身份组装场景**：渲染端不为所有人预缓存同一场景；派生缓存（§3.2）按 scope 分桶。
- **复用 L34/L50（不新建权限模型）**：L34/L50 = policy definition；D40/L91 = enforcement layer。RBAC 评估结果标准化为 policy-resolution 对象（allowed/denied paths + visibility_policy + telemetry_policy 等），pruning 层 / Scene Engine 只消费、不自算（字段名/schema 实现期定）。
- **Omniverse 路径（L91，Yuri 2026-06-28 裁定）**：Omniverse ＝ 受信「全量 USD 工程面」，per-identity pruning **仅作用于 WebGL/客户分发面**；Omniverse 路径由 L81 ⑥ service-token + operator 访问控制把关（不做场景级裁剪），与 L81 ⑥ 一致、不修订 L81。
- **未锁子项（L91 范围说明）**：① fail-closed 粒度（整场景 empty vs per-asset deny + 外壳）= 实现期细化；② ghost 四态 taxonomy（hidden/ghost/aggregated/redacted）与 policy snapshot versioning = 待立项建议（与「visibility_policy 三值」收敛后另议）。
- 鉴权身份来自门户会话（D31）；过滤 scope 复用 spec-004 / RBAC 既有 path-glob，不新建权限模型。
- 依据：`docs/D38-D41-render-decisions.md` D40。

---

# 4. 渲染器契约（Layer 3，WebGL 与 Omniverse 共享约定）

两个渲染器消费同一场景，**共享下列约定**（单一权威，禁止各渲染器各写一套）：

## 4.1 着色 / 阈值约定（D41 → **L92 锁定 2026-06-28**：语义→视觉确定性映射协议，单一权威本节，收口 spec-007 Q4）
- **本质 = 确定性映射函数（L92）**，非配色/UI 主题：`F:(quantity 值, severity, context) → visual_state`。状态/温度/告警 → **渲染器无关的语义映射**（色带停靠点、告警→强调等级），定义为**数据**，WebGL 与 Omniverse **同读**——避免两渲染器视觉漂移。
- **visual_state 契约（L92，渲染器无关）**：输出标准视觉状态对象（如 color/opacity/emissive/animation/geometry_modifier/priority——**字段示例，权威 schema 实现期定**）。**WebGL = visual_state → shader params；Omniverse = USD material override layer**。契约**带版本（versioned，L92）**。
- **禁渲染端覆盖（L92 红线）**：两渲染器**只消费、不定义**映射；**禁** UI override、禁任一渲染端本地 shader/material 自定义映射；**渲染器限制 MUST NOT 反向改阈值/severity**（视觉受限只降级表现，不动语义）。
- **词汇复用（硬 CI）**：连续色带键于 spec-002 `quantities.yaml` / `units.yaml` 量纲单位；离散强调键于 **spec-003 告警 severity**——**不发明新量纲 / 新等级**（守 protocol 词汇互斥 CI）。
- **阈值归属（L92 边界）**：告警阈值 / severity 分级**留在 spec-003**（telemetry 产值、spec-003 分级）；**D41 只拥有「视觉色带停靠点」+「severity→强调」映射，不复制告警阈值**。hysteresis 仅作**视觉防抖**（值在停靠点附近不频闪），**MUST NOT 重分类 severity**。
- **载体**：先内联本节语义映射表，证明后再考虑抽为独立机读 yaml（仿 protocol/*.yaml；若抽 yaml 须过词汇互斥 CI 且只引用既有条目）——避免提前抽象。
- **阶段（同 L90 phasing 说明）**：2D 状态着色（severity→色/徽标）自 **Phase A** 适用；完整空间视觉编码（emissive/pulse/outline/heatmap）= **Phase B**；「状态视觉皆派生自本节、禁直接 color injection」enforcement 在任何有视觉处恒定。
- spec-007 Q4 已关闭并指向本节（spec-007 v0.3 / L92）。
- 依据：`docs/D38-D41-render-decisions.md` D41 + `docs/LOCKED.md` §28（L92）。

## 4.2 对象明细投影
- 节点 → R8 检视字段（ID / 名称 / Status / 进出口 Temp{press, flow} / Alarm / action）的取数路径由 §3.1 遥测/告警绑定给定，渲染器只做呈现。

## 4.3 流动画（R7）
- 拓扑边（spec-001 §7）→ 动画：方向取 `from→to`，**强度/速度由 live 遥测驱动**（电流：实际功率/电流 vs `rated_kw`；冷却：实际流量 vs `rated_cooling_kw`）。`redundancy_group` 用于 A/B 路视觉区分。
- 遥测不可达 → 边降级为静态（保结构，标 stale），不崩场景（同 spec-007 §6 失败软）。

---

# 5. WebGL 运维 NOC — 功能规范（R1–R9）

## 5.1 导航：全局 → Focus → 细节（R1）
- 进入 = site 全局视图；选中对象 → focus（镜头聚焦 + 邻域强调）；再下钻 → 细节面板。三级导航，**数据按层级局部刷新**（§6），非全页 reload。

## 5.2 异常下钻（R3，Starlink NOC 风格）
- 告警节点高亮（§4.1 约定）→ 点击 → "查看原因"（根因/影响面：复用 spec-001 §7 feeds/cools 图 + spec-003 告警）→ 进入分析与操作面板（操作 = M3 后段/M4，见 §8）。

## 5.3 对象数据标签（R4）
- 对象下方贴关键数据：运行时间 / Power / State / Utilization 等。字段取自 §3.1 遥测绑定；标签随 §6 增量流刷新。

## 5.4 站点信息 chart（R5）
- 页面右下角曲线：Facility Power / IT Power / PUE 等**站点级派生量**。数据源 = spec-004 `/v1/metrics/query`（站点级派生量，注意 L48「PUE 仅 site 级」）。

## 5.5 站点切换器（R6）
- 多站点 ORG：左上角透明下拉框切 site（ORG/多站权限对齐 D35 Org 层级）。下拉项若该站有 firing 告警 → 背景标注醒目提醒色（数据 = spec-003 站点级告警聚合）。

## 5.6 对象检视器（R8）
- 点击/悬停 → ID / 名称 / Status / 进出口 Temp{press, flow}（spec-002 supply/return 侧表达）/ Alarm / action。action 在 M3 一阶段为只读占位（§8）。

---

# 6. 数据流与局部刷新（R1）

- **结构一次加载**：场景几何/拓扑/布局加载一次（Scene Engine；几何 = HTTP 分块/3D-tiles，§3.2）。
- **遥测增量流（D39 已决 2026-06-22）**：测点值以增量推送刷新对应节点属性与标签——**非全页 reload**。**协议 = SSE（单向 server→client），服务端以既有 NATS JetStream 为源**，Scene Engine/门户订阅后 fan-out 到 SSE。WebSocket（双向）留给 M4 交互/Set 阶段，不提前引入。刷新节流与设施侧采样对齐（~1–5s 显示足够，与 L52 GPU 高频解耦，同 spec-007 Q3）。
- **契约前置**：SSE 传输契约建议**自 Phase A（M3.5）即定稳**，延续到 closeout 不返工。
- **失败软**：流/查询不可达 → 节点保上次值 + 标 stale，不崩界面。
- 依据：`docs/D38-D41-render-decisions.md` D39。

## 6.1 SSE↔NATS 传输契约（D39 细化，**locked 2026-06-26 Yuri**）

> 此前 D39 只锁"协议 = SSE / NATS 为源"，未给 subject 与载荷形状（PRMT-119 据此 pre-flight halt）。下列各项**不发明新约定**——subject 与载荷皆引用既有权威源，仅把 Event 映射方向拍定。Gateway 进程内 NATS→SSE 桥的实现门控（PRMT-119）据此解除。

- **Subject（权威源 = spec-006 §2.2，已在 `pkg/natspub/types.go` `TelemetryBatch.Subject()` 落地）**：`cios.tlm.<site>.<top_asset>`。SSE 桥按 site 过滤 = 订阅 `cios.tlm.<site>.>`（通配 top_asset）。**不引入新 subject 命名**。
- **Stream**：`CIOS_TLM`（edge-writer 既有 stream）。
- **Consumer**：**每条 SSE 连接一个临时（ephemeral）过滤型 push consumer**，`FilterSubject = cios.tlm.<site>.>`，`DeliverPolicy = DeliverNew`（只取连接后的新消息，不回放历史）。**禁止用 durable**——会与 edge-writer 的 `edge-writer` durable 抢消费、且随连接累积。连接断开（ctx 取消）即 unsubscribe 销毁。
- **载荷映射（Yuri 2026-06-26 拍板）= 原样转发**：核心发布的 `TelemetryBatch` JSON（`{site, top_asset, timestamp, encoding:"promtext", lines:[…]}`，见 `pkg/natspub/types.go`）作为 SSE `Event.Data` **逐字转发**，Gateway **不解析 promtext、不拆 per-sample**。门户侧自行解析。理由：守 §7.1 红线（Gateway 只携带身份、不解释数据），耦合最小，无 spec-002 单位耦合。
- **注入连接类型**：`nats.JetStreamContext` 的最小订阅子集（仿 `pkg/natspub` 的 `JetStreamContext` interface 模式，仅暴露 `Subscribe`）；连接在进程内构造（装配属 PRMT-111+），**Portal 不直连 NATS（§7.1 / L81 红线）**。
- 依据：`docs/D38-D41-render-decisions.md` D39（含 2026-06-26 细化）、spec-006 §2.2、`pkg/natspub/types.go`。

---

# 7. 安全（对齐 spec-006 §5）

- **RBAC 服务端强制**：§3.3 越权资产仅轮廓、零明细，服务端过滤。
- **传输/审计**：门户 ↔ Gateway ↔ Scene Engine ↔ /v1 走 mTLS + 审计（spec-006 §5）；鉴权架构见 §7.1。
- **只读**：M3 一阶段无写路径；任何 action/Set 透传 = M3 后段/M4，独立契约 + 安全评审 + 双人审批（spec-002 set 动词 + spec-006 §5），见 §8。

## 7.1 鉴权架构（D31 已决 2026-06-22；**L81 已锁 2026-06-23**，见 `docs/LOCKED.md` §18）

**适用面**：全体验层表面——E3.5 运维门户 + E3.4 客户门户 + Omniverse + CLI。**核心原则（Yuri）**：OIDC 负责"人是谁"（authn）/ STS 负责"拿什么 token" / Policy Engine 负责"能干什么"（authz）/ Omniverse 只认 service token，不认 session。

**逻辑隔离（非强制物理；运维 vs 客户）**
- 单 IdP + **双鉴权上下文**：`ops-realm` / `customer-realm`。**与 D32（门户分开部署）兼容**——IdP 是共享鉴权 infra，两门户仍各自部署、各连各自 realm。
- 与 D33（多租户隔离深度，**L83 已锁 2026-06-23 = 分层隔离（库/行/标签按租户分类，默认标签）**）/ D35（Org 层级，**L84 已锁 2026-06-23 = 引入显式 Org 对象，tenant→Org→site**）协同：realm 之下的租户/Org scope 由下述 Policy Engine（上下文维）+ RBAC（资源维，core 权威）表达；tenant/Org 为叠加判别维，资源 scope 真相不离 core（守 L81 红线）。

**IdP（D31 = Yuri "all"：IdP-agnostic）**
- 标准 OIDC，**provider 无关**；部署可选 **Keycloak / Auth0 / Cognito**（任一）。realm 概念 provider-mapped（Keycloak realm / Auth0 organization / Cognito user pool）。
- IdP **只做 OIDC authn**（人是谁），不承担 token-exchange/授权。

**STS（gateway 侧，IdP-agnostic）**
- Token Exchange 置于 **Gateway 侧**（**不**假设 IdP 原生 RFC 8693——Keycloak 支持、Auth0/Cognito 不一致；gateway 侧 STS 才能保持 IdP-agnostic）。
- 流程：**Customer Portal**：OIDC login → session cookie → STS exchange → **customer API token**。**Ops Portal**：OIDC login → session cookie → STS exchange → **ops API token**。
- CLI（spec-004 既有 bearer）可经同一 STS 统一签发。

**API Gateway（唯一入口；Portal 绝不直连 infra）**
- 外部面 `/api/*`，**消费 core `/v1/*` 内部**（spec-004）：
  - `/api/sites` → 聚合 `/v1` 只读（资产/遥测/告警）。
  - `/api/twins` → §3 Scene Engine（场景/几何/绑定）。
  - `/api/omniverse` → Omniverse 流式 broker。
- §6 SSE 遥测增量流亦经 Gateway 中转（Portal 不直连 NATS/infra）。
- 红线：**不让 Portal 直接调用 Infra API**（与 §0 下游消费一致）。

**Policy Engine（D31 = Yuri：新组件 OPA/Cedar）——边界红线（防双权威）**
- Policy Engine = **Gateway 处的 PDP**（policy decision point）；输入 = OIDC claims + **RBAC scope（L34/L50）** + 上下文属性（realm / action / tenant / MFA / time）→ allow/deny。
- **既有 path-glob RBAC（L34 子树语义 / L50 读隐含写显式）仍是资源 scope 的唯一权威**，在 core/`/v1` 强制不变。Policy Engine **消费** RBAC scope 作为输入、在其上叠加**正交的上下文策略**，**不复制、不覆盖**资源 scope 真相。
  - 即：**RBAC = "哪些 crn 子树"**（资源维，core 权威）；**Policy Engine = "在什么 realm/action/上下文下放行"**（上下文维，gateway PDP）。二者不重叠、不互为冗余真相。
- 产品（OPA vs Cedar）= 实现期细化；倾向 OPA（Rego，自托管/sidecar 契合 L78 去中心化）。
- **架构者备注（诚实标注）**：Policy Engine 是**净新增组件**（相对"复用既有 RBAC"方案）；其价值依赖上述边界严格成立——一旦 OPA 开始承载资源 scope，即退化为与 RBAC 的双权威，须在评审守住边界。

**Omniverse service token**
- `/api/omniverse` 以**机器身份 service token** 调 Omniverse/Nucleus；**不**透传 user session（贯彻 Yuri 原则）。

**零侵入（守 §0）**
- Gateway / STS / Policy Engine 皆**体验层新增组件**；**core RBAC（L34/L50）与 spec-006 §5 不改**——本节是其下游叠加，不改宪法、不动 frozen spec。

---

# 8. 分期（Yuri 2026-06-22：先轻量功能验证，渲染后置为收尾）

**核心定序：先用轻量页验证「接口 + 数据 + 所有功能点」，把昂贵的空间渲染拆为独立收尾阶段。** 这是 backbone-first 在体验层的延续——轻量页只是又一个 `/v1` 下游消费者，**不需要 Scene Engine / WebGL 3D / Omniverse**，因此**不被 D37/D38/D39/D41 阻塞**；后者全部下移为 closeout 门控。

## Phase A — M3.5：轻量功能/数据验证（light web）
- **形态**：薄 `/v1` 消费者（TS/React，D30），2D/表格/简单 SVG/曲线。**无 Scene Engine、无 WebGL 3D、无 Omniverse、无空间渲染。**
- **验证目标（所有功能点的功能内核）**：
  - 接口：`/v1/assets`、`/v1/metrics/query`、`/v1/alarms` 的契约、形状、错误处理。
  - 数据：遥测/告警取数正确；**RBAC 过滤正确**（越权资产数据不可见，经既有 `/v1` path-glob RBAC，L34/L50）。
  - 功能点：R1 导航逻辑（全局→focus→细节，列表/树形式）、R3 异常下钻逻辑（高亮→查看原因→根因/影响面，用 spec-001 §7 图 + spec-003）、R4 对象数据、R5 站点 chart（Facility/IT Power/PUE，L48 site 级）、R6 站点切换 + 异常站醒目标注、R8 对象检视字段。
- **本阶段不含（属渲染，移 Phase B）**：R2 透明轮廓空间呈现、R7 Topology+Flow 动画、focus 的空间镜头/三维下钻。
- **门控**：D30（前端栈）+ D31（门户鉴权，§7.1）+ 既有 `/v1` RBAC——**均已满足 ✅，可开工**。**不需 D37/D38/D39/D41。**

## Phase B — M3 closeout：渲染（空间数字孪生）
- **形态**：§3 Scene Engine + §4 渲染器契约落地——**WebGL 空间渲染器**（R2 透明轮廓、R7 流动画、focus 空间镜头）+ **Omniverse**（spec-007）。
- 复用 Phase A 已验证的接口/数据/功能内核，叠加空间渲染表面。
- **门控**：D37（L80 锁）+ D38（场景格式）+ D39（传输）+ D40（Scene Engine 几何级 RBAC 过滤，原则自 Phase A 起恒定）+ D41（统一着色）。详见 `docs/D38-D41-render-decisions.md`。

## 跨阶段恒定
- **交互控制（M3 后段 / M4）**：异常下钻后的"操作"、对象 action、Set 透传回 CIOS（spec-002 set + 审计 + 双人审批）。与 spec-007 §8 / Q9 同源，**另行起草，需安全评审**。Phase A/B 均只读。
- **视图切换（R9，GPU view）= M3 不启用**：占位入口保留，启用对齐 D29（AI fabric 随 GPU 一起做）/ L68（GPU/IT 遥测后置）。
- **里程碑命名（待 Yuri 确认）**：Phase A = M3.5、Phase B = M3 closeout 为 Yuri 2026-06-22 提议；正式 M 标签 + milestone-bar HTML / M-plan 同步 = 确认后单独执行（守 §5 M# 同步纪律）。

---

# 9. 与 spec-007 的关系（避免重叠混乱）

- **spec-009 = Experience Layer 架构 + Scene Engine 契约 + WebGL 渲染器**（本规范）。
- **spec-007 = USD 映射细节 + Omniverse 渲染器 + 型号包 + Site-Draw 布局**（Omniverse 渲染器专属）。
- 二者通过 §3 Scene Engine 衔接：spec-007 的 USD stage / Site-Draw 布局是 Scene Engine 的场景/布局来源之一；§4.1 着色约定统一覆盖两个渲染器（D41 取代 spec-007 Q4 局部定义）。
- **交叉更新（待 D37 锁后）**：spec-007 §1 红线「Web 端不做任何 3D 渲染」需按 L80 细化为「不自研照片级 3D；运维 WebGL 渲染器获准」。spec-007 为 DRAFT，本规范锁定时同步升版。

---

# 10. 未决问题（评审拍板 → 升 v1.0 + L 号；实现门控）

| # | 问题 | 对应 D | 架构者倾向 | 状态 |
|---|------|--------|-----------|------|
| Q1 | 三层架构 + L45 细化（WebGL 渲染器获准）| **D37** | — | **已决 → L80（2026-06-22 锁）** |
| Q2 | Scene Engine 场景格式 + WebGL 取几何管线 | D38 | — | **已决 2026-06-22**：USD 为源，单向转码 web 原生（glTF/3D-tiles + JSON），§3.2 |
| Q3 | 场景/遥测传输到 web | D39 | — | **已决 2026-06-22**：遥测增量 = SSE（NATS 为源）；几何 = HTTP/3D-tiles，§6 |
| Q4 | RBAC 场景过滤强制点 | D40 | — | **LOCKED → L91（2026-06-28）**：USD graph-level pruning（渲染器分叉上游）+ 三红线 + two-stream ghost + fail-closed，§3.3。未锁子项见 L91 范围说明。 |
| Q5 | 统一着色/阈值约定（收口 spec-007 Q4）| D41 | — | **已决 2026-06-22**：单一权威 §4.1，引用既有量纲/severity |
| Q6 | 前端栈 / SSR vs SPA | D30 | — | **已决**：TS/React，SSR/Hybrid（Yuri） |
| Q7 | 门户鉴权 | D31 | — | **已决 2026-06-22**：单 IdP（agnostic）+ 双 realm + gateway 侧 STS + API Gateway + Policy Engine(OPA/Cedar) + Omniverse service token，§7.1（**L81 已锁 2026-06-23**） |
| Q8 | 多站/ORG 权限（站点切换器 R6）| D35 | 引入 Org 对象（Yuri 已定需要）| **已决 → L84 锁 2026-06-23**：引入显式 Org 对象，tenant→Org→site（隔离深度 D33→**L83 = 分层：库/行/标签按租户分类**）|
| Q9 | 交互控制 / action / Set 透传契约 | — | 复用 spec-002 set + 审计 + 双人审批；M3 后段/M4 另起草 | 占位 |

> **实现门控（更新 2026-06-22，D31 已决）**：
> - **Phase A（M3.5 轻量功能页）**：门控 D30 + D31 + 既有 `/v1` RBAC **全部满足 ✅**——**可签发 Phase A 编码 PRMT**。
> - **Phase B（M3 closeout 渲染）**：门控 = L80（已锁）+ D38–D41（已决）+ D31（已决）**全部满足 ✅**——待 Phase A 验证完成后启动（守先轻量、渲染后置定序）。
> 本规范独立线，不挤占 M2 退出 / M3 backbone。

---

# 11. CHANGELOG

| 版本 | 日期 | 变更 |
|------|------|------|
| v0.1 DRAFT | 2026-06-22 | 首版草稿：形式化 Yuri 9 项 B/S 交互需求 + 三层渲染架构（Core/Scene Engine/Renderer{WebGL,Omniverse}，D37 提案 L80 细化 L45）。§0 零侵入保证、§2 三层 + L45 细化、§3 Scene Engine（来源/格式/RBAC 服务端过滤）、§4 渲染器共享契约（着色/流动画）、§5 WebGL NOC 功能（R1–R9）、§6 局部刷新数据流、§7 安全、§8 分期、§9 与 spec-007 关系、Q1–Q9 + 门控（D37–D41/D30/D31）。与 spec-001–006/008 零改动、只读下游。**未冻结、无 L 号。**|
| v0.2 DRAFT | 2026-06-22 | **D37–D41 拍板折入**：D37 → **L80 锁定**（§2、上位决策；spec-007 §1/Q4/上位决策同步至 v0.3）；D38 → USD 为源单向转码 glTF/3D-tiles + JSON（§3.2）；D39 → 遥测增量 SSE（NATS 为源）+ 几何 HTTP/3D-tiles（§6）；D40 → 服务端强制过滤 + 轮廓可见/敏感租户隐藏开关（§3.3）；D41 → 单一着色权威 §4.1 引用既有量纲/severity。§10 Q1–Q6 标已决；门控更新为分阶段（Phase A 待 D31；Phase B = L80+D38–D41+D31）。仍 DRAFT（D31/Q9 未决）。|
| v0.3 DRAFT | 2026-06-22 | **D31 鉴权架构折入（新 §7.1，提案 L81）**：单 IdP（IdP-agnostic：Keycloak/Auth0/Cognito）+ 双 realm（ops/customer，逻辑隔离，兼容 D32）+ **gateway 侧 STS**（IdP-agnostic token exchange）+ **API Gateway**（`/api/sites|twins|omniverse` 消费 `/v1`，Portal 不直连 infra，brokers SSE）+ **Policy Engine OPA/Cedar**（gateway PDP，消费 L34/L50 RBAC + 上下文；边界红线：RBAC 仍资源 scope 唯一权威）+ **Omniverse service-token-only**。Q7/D31 标已决；**Phase A 门控 D30+D31+RBAC 全满足 ✅（可签发编码 PRMT）**。core RBAC / spec-006 §5 不改（零侵入）。仍 DRAFT（Q9 占位；L81 待锁措辞）。|
| v0.4 DRAFT | 2026-06-23 | **L81 锁定**（D31 鉴权架构，`docs/LOCKED.md` §18）：§7.1 去「提案/待锁」标记。据此签发 `feature/m3-auth` 首批编码 PRMT-101–110（鉴权骨架 8 个可执行 + 租户 scoping 2 个 OPEN 待 D33）。spec 仍 DRAFT（Q9 控制透传占位；§3/§4 渲染待 Phase B）。|
| v0.5 DRAFT | 2026-06-28 | **D38 升锁 → L90**（`docs/LOCKED.md` §26）：§3.2 标 L90 锁定，去「confirm」态。L90 并锁四层职责（USD=SSOT / Omniverse=真相渲染器 / Scene Engine=转码层 / WebGL=分发层）+ 三红线（WebGL 不直解 USD / Omniverse 不参与分发 = golden reference viewer 非 runtime dep / 越权不下发归 D40）+ WebGL 为 CIOS 必需（edge+multi-tenant+remote ops）。**D39 已于 2026-06-26 升 L87；D40/D41 仍 confirm 折入本 spec、未单独给 L 号**。Yuri 评论 M3/M3.5/M4 phasing 与本 spec §8 Phase A/B 命名冲突，未编码（见 L90 范围说明）。spec 仍 DRAFT（Q9 占位；§4 渲染待 Phase B）。|
| v0.6 DRAFT | 2026-06-28 | **D40 升锁 → L91**（`docs/LOCKED.md` §27）：§3.3 重写——强制点明确为 **USD graph-level pruning 层（在 Scene Engine/渲染器分叉上游，Scene Engine 不评估权限）**；选方案 C，淘汰客户端/混合过滤；新增三红线（Scene Engine 不评估 / 渲染端不二次过滤 / 全量 USD 永不下发）、two-stream ghost（零遥测、禁 click/metadata/timeseries）、fail-closed（deny 绝不全量）；L34/L50 = policy definition、D40 = enforcement layer，policy-resolution 对象消费而非自算。Q4 标 L91。**Omniverse 路径已裁定**（Yuri）＝受信全量 USD 工程面，pruning 仅 WebGL/客户分发面（与 L81⑥ 一致）。**未锁子项**：fail-closed 粒度 / ghost 四态 taxonomy + policy snapshot versioning（Yuri §7 建议，待立项）。**剩 D41 仍 confirm 折入、未单独给 L 号**。spec 仍 DRAFT。|
| v0.7 DRAFT | 2026-06-28 | **D41 升锁 → L92**（`docs/LOCKED.md` §28）：§4.1 重写——D41 = 语义→视觉确定性映射协议（`F:(quantity,severity,context)→visual_state`），非配色/UI 主题；单一权威、渲染器无关 visual_state 契约（WebGL→shader params / Omniverse→USD material override）、**versioned**、**禁渲染端覆盖**、渲染器限制不得反向改语义。词汇仍键于 quantities/units + **spec-003 severity**；**阈值归属明确：告警阈值留 spec-003，D41 仅拥有视觉色带停靠点，hysteresis 仅视觉防抖不重分类**。**架构者更正 Yuri 草案**：severity 权威 = spec-003（非 D40）。Q4/spec-007 指向本节标 L92。**至此 D38–D41 全部升锁**（D38→L90/D39→L87/D40→L91/D41→L92）。spec 仍 DRAFT（Q9 占位）。|
