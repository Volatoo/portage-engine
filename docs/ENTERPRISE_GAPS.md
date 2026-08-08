# 企业内部构建中心差距分析与行动清单

更新日期:2026-08-08

本文件面向企业内部构建中心这一目标，与面向公网 Public Beta 的
[NEXT_STEPS.md](NEXT_STEPS.md) 互补：后者关注公网加固与现场 Gate,本文关注
CI 集成、身份治理、审计合规、构建自动化和平台化部署。结论基于对
`internal/iam`、`internal/server`、`internal/builder`、`internal/catalog`、
`internal/capacity`、`internal/notification`、`deploy/` 的逐文件核实。

## 已具备、无需重做的能力

- 离线/内网架构:egress 策略是目录一等对象并在 PVE 防火墙层强制
  (`internal/iac/pve_egress.go`),镜像工厂在断网命名空间内构建，内部 git
  镜像是默认配置形态(`configs/catalog.example.json`)。
- 项目配额引擎：队列/并发/日限/vCPU/内存/磁盘/构建秒数/云成本/失败风暴冷却
  全部在 PostgreSQL 事务内强制(`internal/persistence/usage_budgets.go`)。
- 调度与一致性：`FOR UPDATE SKIP LOCKED` 认领、项目公平虚拟运行时、租约
  fencing、跨副本吊销、step-up、back-channel logout。
- 供应链与恢复:digest 锁定基础镜像、SBOM、cosign、pgBackRest/PITR 演练
  脚本、告警 runbook(`docs/OBSERVABILITY_RUNBOOKS.md`)。

## 差距总览

| 领域 | 差距 | 证据 |
| --- | --- | --- |
| CI 身份 | 无 service account、无 workload federation、session 无刷新；流水线只能持全局 `API_KEY`(等于 system-admin) | `internal/server/iam.go:263-289`,全仓库无 `client_credentials` |
| 审计 | `audit_events` 只写不读：无查询 API、无导出、无保留策略、无防篡改 | 生产代码无一处 `SELECT audit_events`;路由表 `internal/server/server.go:1089-1170` 无 `/api/v1/audit*` |
| 成员管理 | 多 provider 部署下成员授权全部 400;无 CLI/UI | `internal/server/iam.go:1247` 用 legacy `OIDC_ISSUER_URL` 校验 issuer |
| 目录集成 | 无 LDAP/AD/SAML/SCIM,不消费 group claim(有意设计),入职/离职逐人手工 | `internal/iam/iam.go:32-33`、`docs/IAM.md:15-16` |
| 权限层级 | 项目扁平，建项目仅 system-admin,system-admin 二元且改动需重启 | `internal/server/iam.go:1172-1176`、`configureIdentityAdmins` |
| step-up | GitHub 身份无 `auth_time`,永远无法通过 step-up | `internal/iam/github.go:117`、`docs/IDENTITY_PROVIDERS.md:209-214` |
| 构建触发 | 全部人工单发；无定时/周期构建，无 GLSA/新版本/revdep 自动重建 | 路由表无 schedule/trigger 端点；`internal/` 无 `glsa|revdep|subslot` 匹配 |
| 批量构建 | `ConfigBundle` 支持 128 包批量，但 CLI 逐包提交；`@world`/set 语法被 atom 校验拒绝 | `cmd/client/main.go:1042-1052`、`internal/builder/validate.go:46` |
| catalog 运维 | 启动时加载一次，变更需换文件加重启;overlay 内容变更需重建镜像；无 CRUD API | `internal/server/server.go:600`、`docs/CATALOG.md:29,80-84` |
| 通知 | 渠道仅 builder 侧单全局配置；无签名/重试/投递账本/每项目订阅;server 不发事件;IRC 是 stub | `internal/builder/local.go:429,840`、`internal/notification/notification.go:308-325` |
| 依赖闭包 | 镜像已装依赖不重建不发布，安装验证只证明相对镜像基线可装；`--autounmask-continue` 静默接受 masked 版本 | `internal/builder/local.go:1513-1518`、`internal/builder/executor.go:162-207` |
| 存储治理 | `cmd/artifact-lifecycle` GC 正确但无任何调度；配额仅按单次 attempt,无项目累计发布字节配额 | `cmd/artifact-lifecycle/main.go` 头注释 |
| 部署形态 | 无 Kubernetes 路径;Compose HA overlay 固定两副本 | `deploy/` 全目录；仓库无 Helm/manifest |
| 升级 | `MinSchemaVersion == MaxSchemaVersion == 30`,schema 升级必须全 drain 切换 | `internal/persistence/database.go:29-30`、`docs/PVE_TESTING.md:805-809` |
| provider | autoscale 路径只注册 PVE;egress 强制绑定 PVE 防火墙 API | `cmd/capacity-actuator/main.go:109`、`internal/capacity/pve.go` |
| 规模 | 无吞吐基准；最大验证拓扑 2 副本 + 1 VM;无 warm pool,每次构建 VM 克隆冷启动 | `docs/PVE_TESTING.md:812-816,1030-1034` |
| 可观测 | Tempo/OTLP 管道已部署但 Go 代码零 instrumentation;日志非结构化 tail | `go.mod` 无 otel 依赖 |
| 架构 | Gentoo 构建实际 amd64-only;image factory 硬拒其余组合 | `internal/imagefactory/plan.go:247`、`catalyst.go:18` |

## P0:企业接入阻塞项

### 多 provider 成员管理

- [ ] 修 `internal/server/iam.go:1247`:issuer 校验改为查 provider registry,
  而非 legacy `OIDC_ISSUER_URL` 单值；补多 provider 授权回归测试。
- [ ] `portage-client` 增加 `project-create`、`member-add`、`member-remove`、
  `member-list` 子命令；或在 Dashboard 提供成员管理页面。
- [ ] 提供按 provider + username/email 查询 `sub` 的辅助接口，授权不再要求
  管理员手工获取不透明 subject 值。

退出标准:Authentik + GitHub 双 provider 部署下，能通过 CLI 或 Dashboard
完成建项目、授权、回收的全流程，不使用 curl。

### CI 一等身份

- [ ] 设计并实现 OIDC workload federation:server 校验 GitLab CI / GitHub
  Actions / Jenkins 签发的 OIDC token(issuer/audience/subject 声明映射到
  project),换取 project-scoped 短时限 `pe1_` session。
- [ ] 备选或过渡:project-scoped machine token,持久化仅存 hash,可列出、
  可单个吊销、可设过期，权限上限 `developer`。
- [ ] 流水线路径全程不接触全局 `API_KEY`;文档给出 GitLab CI 与 GitHub
  Actions 的接入示例。

退出标准：一条真实流水线在不持有任何 system-admin 凭证的情况下提交构建、
轮询状态并拉取产物;token 泄露的爆炸半径不超过单个 project 的 developer 权限。

### 审计查询与导出

- [ ] 增加 `/api/v1/audit` 查询 API(system-admin + step-up),支持按时间、
  actor、action、project、outcome 过滤与分页。
- [ ] `audit_events` 按时间分区或增加保留裁剪任务，保留期可配置。
- [ ] 复用 `outbox_events` 模式实现审计事件导出通道(webhook/SIEM),
  投递失败可重放。
- [ ] Dashboard 增加审计视图(至少 system-admin 可见)。

退出标准：安全团队不接触数据库即可检索与订阅审计事件；保留策略生效且
有裁剪证据。

## P1:构建中心核心日常能力

### 批量与集合构建

- [ ] CLI 走 `ConfigBundle` 的批量路径，一次提交最多 128 包为单个 job
  (`internal/builder/config_transfer.go:48-65` 已支持，`cmd/client` 未接)。
- [ ] server 侧引入可审核的 named package set 目录对象，作为提交单位；
  set 展开在 server 侧完成，不放宽 `atomPattern` 对 `@` 的拒绝。

### 重建自动化

- [ ] 第一步只做差距报表:binhost 当前包集对比 GLSA 公告与镜像仓库新版本，
  输出受影响包清单，不自动动作。`MirrorBundle.AdvisoryWatermark`
  (`internal/catalog/catalog.go:120`)可作为公告基线输入。
- [ ] 第二步在报表之上做审核后自动排队：操作者确认清单，系统批量提交并
  沿用现有配额与公平机制。
- [ ] 定时构建:server 内建 cron 表，持久化在 PostgreSQL,复用现有
  lease/fencing 防多副本重复触发；`PlanRebuild`
  (`internal/imagefactory/operations.go:500-533`)的 interval 策略形状
  可直接迁移。

### catalog 运维减负

- [ ] catalog 热加载：带版本校验的 reload API 或 SIGHUP，替换换文件加
  重启的流程；加载失败保持旧目录并告警。
- [ ] 文档化 overlay bump 快捷流程：内部 ebuild 仓库提交 → 目录 revision
  更新 → 镜像重建 → 验证，给出每步命令与耗时预期。

### 存储与通知治理

- [ ] 为 `cmd/artifact-lifecycle` 提供调度载体(systemd timer 与 k8s
  CronJob 两份参考配置),GC 不再依赖操作者记忆。
- [ ] 增加项目级累计发布字节配额，与现有单次 attempt 配额并存。
- [ ] 每项目 webhook 订阅:HMAC 签名、重试、投递账本；事件源挪到 server
  侧,builder 侧现有实现(`internal/builder/local.go:840`)降级为消费者
  之一；删除或实现 IRC stub。

### 依赖闭包声明

- [ ] 在发布元数据中记录构建所用镜像基线(image generation 与其 VDB 包集),
  消费端可判断闭包是否对本机自洽。
- [ ] 评估 `--autounmask-continue` 的替代：至少把 autounmask 实际改动写入
  构建产物元数据，供审计；或改为 fail-closed 并要求目录显式声明 keywords。

## P2:规模化与平台化

### Kubernetes 部署

- [ ] Helm chart 或 kustomize:server 多副本 Deployment、signer 独立
  StatefulSet + 私有卷、migrate 作为 Job、actuator 与 artifact-lifecycle
  作为 CronJob;PostgreSQL 假定外部提供。
- [ ] 结合现有 k8s 运行经验落地约束:RWO 存储边界、arm64 节点上交叉构建
  amd64 镜像的产线。

### 升级窗口

- [ ] 实现 `MinSchemaVersion < MaxSchemaVersion` 的 N/N+1 兼容窗口，允许
  滚动升级；或短期先交付带门禁的 drain 升级 runbook
  (`docs/PVE_TESTING.md:805-809` 已记录必要性)。

### 吞吐与冷启动

- [ ] 压测:10+ 并发 job,记录 VM 克隆耗时、emerge 耗时、调度认领延迟、
  数据库负载，产出首个吞吐基线文档。
- [ ] 依据数据评估 linked-clone/快照 warm pool 是否值得投入；当前每次构建
  VM 克隆冷启动在关键路径上(`docs/PVE_TESTING.md:1030-1034`)。

### 可观测补全

- [ ] 引入结构化日志与 OpenTelemetry instrumentation(server 关键路径：
  提交、认领、phase 切换、发布),让已部署的 Tempo 管道有生产者；
  或从 Compose 栈移除 Tempo,不保留空管道。

### 多架构与 provider

- [ ] arm64 构建路径：放开 `internal/imagefactory/plan.go:247` 的
  pve/amd64 硬编码，用 native arm64 worker;不做 cross/qemu,与
  native emerge 设计保持一致。
- [ ] 接入第二 provider(如 libvirt)前，先把 egress 强制从 PVE 防火墙
  API(`internal/iac/pve_egress.go`)抽象为 provider 契约的一部分；
  `capacity.Provider` 两方法接口本身已够干净。

### 按需项

- [ ] LDAP/SCIM/组到角色映射：是否实现取决于目标企业的目录形态；实现时
  保持可变 claim 不授权的原则，把目录同步落为 PostgreSQL membership。
- [ ] GitHub 身份 step-up 替代：平台原生 WebAuthn/TOTP,解除
  `internal/iam/github.go:117` 无 `auth_time` 的限制。
- [ ] system-admin 拆分：细分 builder 管理、设置读写、审计读取等
  能力，支持运行时增删管理员而非改环境变量重启。

## 推荐执行顺序

1. P0 三项：成员管理 bug 与 CLI、CI 身份、审计查询与导出。它们决定平台能否被
   提交者之外的第二个角色(流水线、安全团队)使用。
2. 批量构建 CLI 与差距报表：投入小，立即改变日常使用形态。
3. 定时构建与 webhook 治理：补齐无人值守能力。
4. k8s 部署与升级窗口：与现网 k8s 环境对齐，消除升级停机。
5. 压测之后再决定 warm pool、arm64 与第二 provider 的优先级。

## 相关文档

- [Public Beta 待办](NEXT_STEPS.md)
- [身份与项目 RBAC](IAM.md)
- [调度与容量](SCHEDULER.md)
- [目录与镜像](CATALOG.md)
- [生产部署边界](PRODUCTION_BOUNDARY.md)
