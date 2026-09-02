# CIOS Spec 004 — API Conventions

> 版本 **v1.1（2026-07-10，随 spec-001 v1.1 同批升版，L101 授权）**｜ 依赖：spec-001 §2/§5bis、spec-002 §8、spec-006 §5
> v1.1：新增 **§6bis 多租户资源名（crn）**——tenant/org 段位（L99 A1）、与 spec-001 §2 点路径的双射、glob 语义沿用 L50、**双语法过渡期 + 观测关窗**（L101 D4）。资源域增 `/v1/orgs`(M3)。**本次唯一破坏性变更**；§1–§8 其余逐字不变
> v1.0 定稿要点：dedup=(method,path,request_id)/TTL 24h/仅成功响应重放；page_token 客户端不得解析（不透明承诺）；L50 Scoped RBAC 无翻案生效；错误码注册表含 upstream-unavailable/bad-request（PRMT-011 追认）
> v0.2：D14 拍板（L50）——scope glob **读隐含子树、写显式匹配点位**
> 覆盖 TODO T9：URL 方案、分页/过滤、错误格式（RFC 7807）、apply 幂等性、Scoped RBAC。
> 站点 API 与 fleet API 同构（fleet 是超集，spec-006 §1.2）；本文约定两者共同遵守。

---

# 1. URL 方案与版本策略

```text
https://<host>/v1/...        # REST; /v1 起步
```

- 资源域（M0 全部定 schema，实现按里程碑）：
  `/v1/sites` `/v1/assets` `/v1/topology` `/v1/points` `/v1/metrics`
  `/v1/alarms` `/v1/events` `/v1/tickets`(M2) `/v1/tenants`(M3) `/v1/orgs`(M3, v1.1/L99) `/v1/clusters`
  `/v1/usage`(M3, **spec-010** Metering & Usage OSS — domain rules live in spec-010; this row only registers the URL under §1–§6 conventions)
- 资源标识 = **级联路径**直接入 URL：`/v1/assets/site01.pod002.cdu000`
  （路径字符集 `[a-z0-9.]` 无需转义；glob 查询经 query 参数，不进 path segment）
- 向后兼容承诺：v1 冻结后字段**只增不删**、语义不变；破坏性变更须开 `/v2`
- fleet API 多站点寻址与站点 API 同形：路径首段即站点，无需额外前缀

# 2. 查询语义

- **指标查询直接代理 PromQL**，不发明查询语言：
  `GET /v1/metrics/query?query=<promql>&time=...`、`/query_range?...`
  （透传 VictoriaMetrics，指标名/标签按 spec-002 §7 投影）
- 点位读写（spec-002 §8）：
  `GET /v1/points/{path}`（当前值+quality）；`PUT /v1/points/{path}:set`
  （body `{value, request_id, ttl}`；返回 `{command_id}`，结果经回读异步确认，
  状态查 `GET /v1/commands/{command_id}`）
- 资产/告警列表过滤：`?filter=<glob>`（spec-001 §2 glob，语义见 §6 注）+
  字段过滤 `?type=cdu&severity=critical` 等白名单参数

# 3. 分页与排序

- **cursor 制**：请求 `?page_size=100&page_token=<opaque>`；响应
  `{items: [...], next_page_token: "..."}`，token 不透明、有效期内可重放
- `page_size` 上限 1000，缺省 100；排序参数 `?order_by=path`（字段白名单），
  缺省按 path 字典序（设备级三位补零保证字典序正确，spec-001 §2）

# 4. 错误格式（RFC 7807）

`Content-Type: application/problem+json`：

```json
{
  "type": "https://cios.dev/errors/path-not-found",
  "title": "asset path not found",
  "status": 404,
  "detail": "site01.pod009 is not registered",
  "instance": "/v1/assets/site01.pod009",
  "request_id": "01HV..."
}
```

错误码注册表（`type` 尾段）：`bad-path`（路径/词汇非法，对应 pkg/cpath 哨兵）、
`path-not-found`、`bad-request`（参数/body 非法的通用项）、`point-readonly`（对 ro 点 Set）、
`risk-approval-required`（C 类待审批）、`limit-exceeded`（A 类限幅）、`conflict`（乐观锁，§5）、
`quota`、`unauthorized`、`forbidden`、`upstream-unavailable`（上游 TSDB/依赖不可达或超时 → 502，
PRMT-011 增补 2026-06-12）。
新增错误码 = 修订本表。`request_id` 全链路透传并入审计。

# 5. 声明式 apply 与幂等

- `cios apply -f xxx.yaml` = `PUT /v1/assets/{path}`（全量声明，服务端 diff）：
  同一文件重复 apply 结果幂等；删除显式用 `DELETE`（带 `--cascade` 确认子树）
- 乐观锁：资源携带 `resource_version`（整数单调递增）；PUT 带回原值，
  不匹配 → 409 `conflict`；不带 = 强制覆盖（仅 admin）
- 变更类请求（Set/apply/ack）必须带客户端 `request_id`（UUIDv7）：
  服务端按 `(principal, request_id)` 去重 24h，重试安全

# 6. AuthN / AuthZ（Scoped RBAC）

- 人类：OIDC（SSO）；机器：scoped API token（可吊销、带有效期与最小范围）
- **权限 = 角色 × 路径范围（glob）× 动作**：

```yaml
kind: RoleBinding
metadata: { name: cooling-team }
spec:
  subjects: [oidc:group/cooling]
  role: operator                 # admin | operator | viewer | tenant(M3)
  scope:
    - "site01.chiller*"          # spec-001 §2 glob
    - "site01.*.cdu*"
```

| 角色 | 允许动作 |
|------|---------|
| viewer | Query（read） |
| operator | Query + Set A/B 类（control:write，C 类发起但需双人审批，spec-006 §5.4） |
| admin | 全部 + apply/delete + RBAC 管理 |
| tenant | 仅 Cluster 成员路径的 Query（M3 启用，范围由 membership 推导） |

- **子树语义按动作区分（L50/D14 已决）**：
  - **读（Query）隐含子树**：scope 模式匹配某资产即覆盖其全部后代与其上点位
    （实现 = `pattern ∨ pattern.**`，组合 pkg/cpath Glob，无需新原语）
  - **写（Set / control:write）不隐含**：模式必须直接匹配**点位地址本身**
    （授权子树写须显式 `pattern.**`，或精确点位模式如 `site01.chiller*.fan*.rpm`）；
    与 risk_class 审批（spec-006 §5.4）构成双闸
- 拒绝优先：多 binding 取并集，无匹配即 403；所有鉴权决策入审计流

# 6bis. 多租户资源名（crn，v1.1，L99/L101）

多租户/多 Org 语境下，RBAC scope 的**规范资源名（crn）**为：

```ebnf
crn      ::= "crn:tenant/" tid "/org/" oid "/site/" sid ( "/" node )*
tid      ::= [a-z][a-z0-9-]{1,30}     ; 租户 id（spec-001 §5bis.1）
oid      ::= [a-z][a-z0-9-]{1,30}     ; Org 名，租户内唯一（spec-001 §5bis.2）
sid, node ::= spec-001 §2 site / node ; 站内层级 = §2 点路径的逐段斜杠展开
```

- **双射**：spec-001 §2 路径 `fra01.pod002.cdu000` ↔ crn 尾部 `site/fra01/pod002/cdu000`。
  §2 点路径语法本身**不变**——crn 只是 RBAC/资源命名层的包装。
- **glob 语义沿用 §6/L50**：`*` 单段、`**` 任意深度，按 crn 段应用（如
  `crn:tenant/acme/org/*/site/*/chiller*` = 该租户全部 Org 全部站点的 chiller）；
  **读隐含子树、写显式匹配点位**（L50）逐字继续生效。
- **越租户红线**：token 的 tenant claim 必须等于 crn 的 `tid` 段；不等 = 403（无例外通配）。
- **双语法过渡期（L101 D4，评审 8/8b）**：v1.0 点 glob scope（如 `site01.chiller*`）在过渡期内
  仍合法，隐式解释为 `crn:tenant/<token-tenant>/org/default/site/` + 首段站点展开，每次使用记
  **弃用警告**。**关窗 = 按观测**：连续 30 天零旧语法使用（指标可证）后关窗，此后旧语法拒绝。
  既有 RoleBinding 由 v1.1 迁移机械改写（前后 diff 入审计），历史审计不回写。
- 实现落地 = 基座/迁移 PRMT（L101 §7② 执行序），本节为规范权威。

# 7. 北向兼容

- `GET /metrics`：Prometheus exposition（组件自身运行指标）
- 业务遥测：Grafana datasource 直连边缘 VictoriaMetrics（只读、viewer token）
- Webhook 事件外发格式 = CloudEvents（spec-003 §1），不另设私有格式

---

# 8. 未决问题

| # | 问题 | 阻塞 |
|---|------|------|
| Q1 | gRPC 面（站内组件间）是否对外暴露还是仅 REST 对外 | SDK 形态 |
| Q2 | PromQL 透传的租户隔离（M3）：label 注入 vs 查询改写 | M3 |
