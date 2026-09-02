# CIOS Spec 005 — Driver SDK & Conformance

> 版本 **v1.0（冻结，2026-06-13 评审会）**｜ 依赖：spec-002 §5/§6（枚举、点表）、spec-006 §4/§5（质量标记、安全）
> 分期（L56）：**接口语义本版冻结**；进程形态 M0=进程内直连（PRMT-008/009 现状）、M1 切 go-plugin 独立进程（§1 为目标形态）；conformance C1–C10 定义冻结，C5/C7–C10 随 M1 驱动线补测
> 覆盖 TODO T10：接口契约、点表校验规则（含 access/risk_class）、conformance 用例集。

---

# 1. 进程模型

- 驱动 = **独立进程**，经 gRPC 接入 gateway（hashicorp/go-plugin 模式）：
  崩溃不影响 gateway 主体，gateway 负责重启（退避重试）与健康监视
- 通信走 **unix socket**，驱动以独立低权限用户运行；设备凭据按驱动最小可见
  （spec-006 §5.2），由 gateway 注入，不落驱动配置文件
- 一个驱动进程服务一种协议（modbus/snmp/mqtt/redfish/...），可绑定多台设备
- 驱动二进制与点表均经型号包签名分发（spec-006 §5.5），gateway 加载前验签

# 2. 接口契约（Go）

```go
type Driver interface {
    // Init 注入配置并建立设备连接; 失败返回 error, gateway 退避重试。
    Init(ctx context.Context, cfg DriverConfig) error
    // Discover 可选自动发现; 不支持的驱动返回 ErrNotSupported。
    Discover(ctx context.Context) ([]AssetCandidate, error)
    // Collect 拉取型协议的一轮采集; 按点表全点位返回(含失败点, 见 Sample.Quality)。
    Collect(ctx context.Context) ([]Sample, error)
    // Subscribe 推送型协议(MQTT/SNMP trap); 拉取型驱动返回 ErrNotSupported。
    Subscribe(ctx context.Context, ch chan<- Sample) error
    // Write 控制点写入(M4 前默认禁用, gateway 侧闸门); 必须支持回读。
    Write(ctx context.Context, cmd ControlCommand) (ControlResult, error)
    // Health 驱动自检: 连接状态/最近成功采集时刻/错误计数。
    Health(ctx context.Context) DriverHealth
}

type Sample struct {
    Point    string    // 完整点位地址 (spec-002 §1, 由 gateway 按点表拼接下发)
    Value    float64
    Ts       time.Time // 设备时标; 无则驱动打采集时标并标记
    Quality  Quality   // good | stale | suspect | substituted (spec-006 §4)
}

type ControlCommand struct {
    Point     string
    Value     float64
    RequestID string        // 幂等键 (spec-004 §5)
    TTL       time.Duration // 过期拒执行 (spec-006 §5.4 防重放)
}

type ControlResult struct {
    Accepted bool
    Readback float64   // 回读值; Set 成功判定在 gateway (spec-002 §8)
    ReadbackTs time.Time
}
```

- 单位换算、enum_map 归一、scale **由 gateway 执行**（点表驱动）；驱动只交付原始值
  → 驱动实现里**禁止**出现量纲/单位逻辑，保证"新设备零代码"
- `Collect` 必须整轮返回：单点失败置 `Quality=suspect` 并继续，不得整轮报错；连接级故障当轮就地重连一次，重连失败 → 剩余点全部 suspect 且仍返回 nil error（设备级故障永不成为 Collect 的 error）
- `Health.LastSuccess` = 最近一次**全轮无 suspect 的 Collect** 完成时刻——Write 成功**不得**刷新（staleness 检测依赖此语义在"控制回路通、遥测死"工况下不被掩盖）（v1.0 回写，PRMT-009 审核定语义）
- 点表 `protocol` 键归驱动私有自治（如 modbus 的 register/table），pointmap 只校验自身已知键（v1.0 确认）

# 3. 点表校验规则（conformance 静态检查）

点表（spec-002 §6）装载前必须通过以下校验，任一失败拒绝加载：

| # | 规则 |
|---|------|
| V1 | `point` 相对路径经 pkg/cpath 校验合法（位置段顺序、量纲在字典） |
| V2 | `unit_in` 必须可换算到该量纲标准单位（换算表内存在） |
| V3 | `enum_map` 值域 ⊆ spec-002 §5 六值枚举；status 点必须提供 enum_map 或声明已是标准码 |
| V4 | `access: rw` 点：`risk_class`（a/b/c）**必填**；A 类必填 `limits{min,max}` |
| V5 | `access: rw` 点：必须存在可回读地址（同 register 可读，或显式 `readback_register`） |
| V6 | 同一点表内 `point` 不重复；`appliesTo` 类型存在于 types.yaml |
| V7 | `source: virtual` 点不得标 rw；derived 点不得出现在点表（spec-002 §6） |

# 4. Conformance Test 套件（动态，驱动合规门槛）

任何驱动必须对 **模拟器 + 标准点表** 通过以下用例才算合规（M0 退出标准之一：
modbus-sim 通过全套）：

| # | 用例 | 验收 |
|---|------|------|
| C1 | Init 成功/凭据错误失败路径 | 错误可区分（auth vs 网络） |
| C2 | Collect 全点位 | 点数 = 点表点数；值与模拟器注入一致（scale 前原始值） |
| C3 | 单点故障容错 | 模拟器屏蔽一个寄存器 → 该点 suspect，其余 good |
| C4 | 断线重连 | 断链期间 Health 报 unhealthy；恢复后自动续采，无需重启 |
| C5 | 推送型订阅（适用者） | trap/消息 → Sample 延迟 < 1s |
| C6 | Write + 回读 | 写后 Readback 反映新值；TTL 过期命令拒绝执行 |
| C7 | Write 禁用闸门 | gateway 未开 control 时驱动 Write 不可达（进程级验证） |
| C8 | 崩溃恢复 | kill -9 驱动 → gateway 重启之，≤10s 恢复采集 |
| C9 | 时钟异常 | 设备时标跳变 → Sample 标 suspect，不污染时序 |
| C10 | 资源上限 | 1000 点 @1Hz 采集 CPU/内存在 pod gateway 预算内（数值待 D10） |

# 5. 驱动生命周期与版本

- 驱动清单（gateway 配置）声明：驱动名、版本、点表引用、采样率、设备端点
- 升级 = 型号包推送新版本（spec-006 §2.3 cfg 下行）→ gateway 原子切换 + 失败回滚
- 驱动 SDK 提供 conformance CLI：`cios-driver-conformance run --driver ./bin/x --pointmap y.yaml`
  （M0 交付套件框架与 modbus-sim，用例随版本只增不减）

---

# 6. 未决问题

| # | 问题 | 阻塞 |
|---|------|------|
| Q1 | 事件触发高频采集（T15）的接口形态：Collect 参数化 vs 独立 Burst 方法 | E1.2 |
| Q2 | Discover 产出的 AssetCandidate 与声明式注册（spec-001 §6）的合并策略（自动建档 vs 仅提示） | M1 |
| Q3 | 非 Go 驱动（厂商 SDK 仅 C/Python）：gRPC 接口语言无关，是否官方维护多语言 stub | 驱动生态 |
