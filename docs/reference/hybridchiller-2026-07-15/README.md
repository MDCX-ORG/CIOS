# HybridChiller 通讯协议回收 — 2026-07-15

| 字段 | 值 |
|------|-----|
| 文件 | [`hybridchiller-comms-protocol-2026-07-15.pdf`](./hybridchiller-comms-protocol-2026-07-15.pdf) |
| 来源 | 厂家今日交付（原名 `通讯协议.pdf`） |
| 对应需求 | **R7** — hybrid-chiller twin vendor requirements (internal) |
| 状态 | 已入库 `docs/reference/`（可 git 跟踪）；**未**立正式 `EXT-NNN` |

## 内容性质

- **Modbus Holding Register** 点表（只读报警位 + 模拟量 + 少量 Only_Write）。
- 双系统 **Sys1 / Sys2**（地址大致 400xx / 401xx）。
- 缩放示例：`x10.0(℃)`、`x100.00(Bar)` — 读数需按 scale 还原工程量。

## 在 CIOS 中的用法（摘要）

```text
PDF 寄存器 →（投影/pointmap）→ CIOS cpath → gateway 采集 → /v1 · live twin 着色
```

- **不要**让 Omniverse 直接读寄存器号。
- **0-1 阶段**：优先只读模拟量 + 故障总信号；**Only_Write 后置**。
- 仍缺：R1 几何、R3 BOM、R4 布局、R6 额定等 — 见厂家配合需求表。

## 下一步

1. 从 PDF 抽 MVP 投影表（10–15 点）。
2. Yuri 批号后可升为 `docs/reference/ext-NNN-hybridchiller/`。
