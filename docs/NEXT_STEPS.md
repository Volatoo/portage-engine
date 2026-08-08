# Portage Engine 后续待办与验收计划

更新日期：2026-08-07

## 当前结论

`codex/next-steps-integration` 已补齐本文件中可以在仓库内安全实现和验证的切片：
persistent executor 模板与 fail-closed Gate、CLI device authorization、公开 edge
与身份 Gate、lease/projection observability、签名 OCI release promotion、恢复演练
框架、GTK/Qt/WebView GUI matrix，以及默认关闭的 Distributed Build Alpha。数据库
authority 为 schema v30，迁移顺序固定为 00027 → 00028 → 00029 → 00030。

真实 PVE、正式 IdP、Vault HA、生产备份/对象存储/signer、真实 distccd、GitHub
发布和 30 天运行窗口仍依赖部署凭据或外部环境；这些 Gate 保持 `not-run`，不能
用 repository/static 结果代替。

| 工作流 | 仓库/本地 Gate | 真实环境 Gate |
| --- | --- | --- |
| Persistent Executor | `scripts/persistent-executor-gate.sh repo` 通过；capacity 删除边界需要 `PORTAGE_TEST_DATABASE_URL`，无数据库时记为 `not-run` | PVE SCHED-2B `not-run` |
| Identity / Public Edge | 配置、Nginx/Compose 与 redacted Gate 通过 | 三个 IdP 与公网主机 `not-run` |
| Recovery | 静态 7 项通过，4 个外部阶段明确 `not-run` | Vault/PostgreSQL/object/signer `not-run` |
| Distributed Build | 单元、race 与 PostgreSQL 并发 Gate 通过 | distccd/PVE/双 job/disconnect `not-run` |
| GUI E2E | 签名候选契约与 GTK/Qt/WebView 场景通过 | 真实 digest/fingerprint/PVE matrix `not-run` |
| Release | workflow/manifest/SBOM/provenance/promotion 契约通过 | GHCR push/sign/promote/rollback `not-run` |

## P0：仓库收口

- [x] 检查本地 `.playwright-mcp/` 浏览器测试输出并加入本地 exclude，避免测试
  日志进入版本控制。
- [ ] 审阅并将 `codex/next-steps-integration` 合并/推送到 `origin/main`。
- [x] 统一路线图中的调度边界描述：v1 以 project 作为公平和配额边界，
  capacity pool 负责 hard routing 与容量隔离，不默认实现 target/provider
  层级公平子队列。
- [x] 在整合分支运行全量 Go/race、release/recovery/persistent/edge Gate 和静态检查。
- [x] 把三个 workflow 与 Makefile 的 golangci-lint 从 `v2.7.2` 抬到 `v2.12.2`。
  golangci-lint 拒绝加载语言版本高于自身构建工具链的模块配置，`v2.7.2` 用
  go1.25 构建，对 `go 1.26.5` 的 go.mod 直接 exit 3，`main` 上的 Lint 与 GoSec
  job 因此一直是红的。
- [x] 把 `go.mod` 的下界从 `1.26.5` 降到 `1.26.4`。Gentoo 稳定版 `dev-lang/go`
  是 1.26.4，`Dockerfile.test` 里 `go mod download` 因此失败，CI 的 Test job
  停在构建测试镜像这一步；同时这个下界会把所有源码构建者推到 `~amd64` 编译器上。
- [x] 让 `release-candidate.yml` 在构建二进制前构建 console bundle。缺它时签名
  发布的 `portage-dashboard` 内嵌空 bundle，对所有 console 路由返回 503，而
  checksum 与 cosign 签名都会如实通过。
- [x] 让 `make build` 在干净检出可用：`web-build` 现在依赖 npm 安装标记文件。
- [x] 修完公测前那批可观测性与一致性缺陷，每项都带一条会变红的门禁：
  - `portage_scheduler_lease_expiries_total` 在 `RuntimeStatus` 两秒超时归零，
    Prometheus 判为 counter reset，`increase()` 因此重算整段生命周期总量，在没有
    任何租约过期时点燃 `PortageEngineLeaseExpiry`。现在按序列保留历史高水位，
    失败的读改为重发上一次读数。
  - `portage_distcc_*_total` 改名为 `portage_distcc_*_last_hour` 并改为 gauge。
    `CompileMetrics` 统计的是一小时滚动窗口，安静一小时就让 counter 下降。
  - Monitor 缓存命中改为纯内存返回。此前每次命中都在同一把锁下重算 source
    watermark——对全部可见终态 job 的聚合，内含相关子查询，无索引可用——每次抓取
    和每个 Monitor 读者都排在一次顺序扫描后面。
  - 启动时先跑 retention 再加载 projection。反过来会让内存里正好留着 retention
    随后隐藏的那批行，一致性检查把差异读成 ledger 分叉，`/readyz` 从启动到下一个
    reconciler tick 之间一直返回 503。
  - 租约过期恢复把计数器写入折叠成每行一次、按固定顺序提交。逐行按 `expires_at`
    顺序更新会让多副本以相反顺序取同两行，40P01 回滚掉的是整轮重排队。
  - distcc allowlist 两侧统一用 `NormalizeAtom` 归约，无法归约的条目在启动时拒绝。
  - 编译租约在准入时过期，`local` 回退策略下不再判整个请求失败，交给 builder 已
    实现的受控本地回退；`blocked` 仍然拒绝。
  - guest agent 的 `wait-accessible` 与 `close` 接受调用方下发的等待预算，不再把
    场景声明的 90 秒截断成硬编码的 60 秒。
  - 中文目录改用中文标点，`set.binsha` 三条字符串进入消息目录，`quota.shadow`
    补上译文。新增两条门禁：中文串里禁止出现紧贴汉字的半角标点，以及译文与原文
    逐字相同的键必须在白名单内。
- [x] 复核上一条的每一条门禁能否真的变红，补掉五条只有断言没有覆盖的：
  - 公网 edge 的 frame-ancestors 只落在 `${PORTAGE_PUBLIC_HOST}` 一个 vhost。
    metrics vhost 完全没有 CSP 与 `X-Frame-Options`，API vhost 的三个 IAM
    location 各写一套 `add_header`，按 nginx 语义替换掉 server 级那套。
    `check_frame_protection` 只读公开 vhost，因此校验的正是唯一正确的那个。
    现在遍历每个做反向代理的 server 块及其自带 header 的 location。
  - `ClaimPhaseWork` 在事务开头就取 phase 计数器行，再去锁 `phase_work_items`；
    恢复循环的取消路径先锁 `phase_work_items`、最后取计数器。两条路径各自有序，
    合起来仍是环。`withDeferredLeaseExpiries` 把计数器写入统一推到事务末尾，
    并以 AST 门禁断言 `recordLeaseExpiry` 只有 `flushLeaseExpiries` 一个调用方。
  - 中文标点门禁的字符类不含括号，八条字符串因此带着半角括号通过，其中
    `set.cicustom` 与 `set.hostkey` 正是上一轮改过逗号、留下括号的那两条。
  - `/readyz` 的结构门禁只守 `handleReadyz`/`handleLivez`/`refuseReadiness`，
    而真正推导对外 reason 的 `readyzLedgerReason` 不在其中，把它的返回值改成
    `last_error` 两条用例照样全绿。现在连同调用点实参与非 `Encode` 的写体一起守。
  - `portage_distcc_slots_total` 是 gauge 却留着 counter 后缀，与上一轮改名要修的
    是同一个缺陷；它声明在 `writeSchedulerPrometheus`，逐条点名的断言够不到。
    门禁改为对处理器输出的每一条 distcc gauge 成立。
- [x] 在干净树上重新生成 `evidence/public-beta/repository-gate.json`。上一份的
  `repository_head` 不在本分支历史里，`working_tree_dirty` 为 true，且其后 nginx
  模板又被改过两次。当前这份 10 项全 pass，`working_tree_dirty` 为 false，
  `repository_head` 指向承载它那次提交的父提交——制品无法描述包含自身的树，这一次
  提交的差异只有制品本身。nginx 模板或 `scripts/validate-public-edge.sh` 再改动时
  必须重跑。
- [ ] 推送后确认 GitHub CI、CodeQL 和安全扫描全部通过。

退出标准：工作区干净，远端 `main` 包含当前提交，CI 全绿，路线图不存在与
当前实现相矛盾的调度描述。

## P1：Persistent Executor 与真实 SCHED-2B Gate

仓库切片已实现；当前只缺真实 PVE 现场验收。

- [x] 构建独立的 persistent-executor Gentoo 模板，不复用一次性 job builder。
- [x] 模板只包含 executor 所需组件，不包含签名私钥、PVE/PBS 管理凭证或 API
  listener credential。
- [x] 通过 SMBIOS/instance metadata 注入数据库生成的 capacity instance ID。
- [x] 启动后主动连接 Worker Gateway，注册精确的 provider、zone、architecture、
  build mode、profile 和 image generation capability。
- [x] repository Gate 验证 actuator 单次精确 create 与幂等/stale fence。
- [x] drain 完成、live-work 删除拒绝和 exact identity 删除边界由 SQL 判定，只在
  设置 `PORTAGE_TEST_DATABASE_URL` 时执行。Gate 会核对该用例的 `--- PASS` 行，
  没有数据库时打印 `NOT-RUN capacity delete boundaries` 而不是继续声称通过；
  CI 的 `operational-gates` job 挂了 PostgreSQL service 并断言这行不出现。
- [ ] 在真实 PVE 完成 scale-up → heartbeat → job → drain → delete，并通过 PVE API
  readback 确认 VM 不存在；当前状态 **not-run**。

退出标准：完成一次真实的 scale-up → heartbeat → 执行任务 → drain → delete；
同时完成“有 live work 时拒绝删除”的负向 Gate，并保存数据库、PVE API、日志和
监控证据。

## P1：Public Beta 生产环境 Gate

### 真实身份提供方

- [x] 实现 Authentik、Google、GitHub provider registry、session idle/max/revoke、
  back-channel logout、step-up 与 equal-email 隔离的 fail-closed real-host Gate。
- [ ] 为正式 HTTPS 域名配置 Authentik。
- [ ] 配置 Google OAuth/OIDC application。
- [ ] 配置 GitHub OAuth App。
- [ ] 分别验证登录、callback、nonce/state、session idle/max lifetime、单 session
  revoke、subject revoke-all 和 logout。
- [ ] 验证 maintainer/owner/system-admin 高风险操作的 step-up。
- [ ] 确认不同 provider 的相同 email 不会被自动合并身份。

退出标准：三个 provider 是等效登录入口；项目权限只来自 PostgreSQL
membership，不来自 email 或可变 group claim。

### Vault 生产化

- [x] 实现 Vault 最小 PKI role、恢复/CA 轮换输入校验与 redacted evidence 框架；
  无真实 Vault 时明确 `not-run`。
- [ ] 部署 Vault HA，并明确 storage、unseal 和恢复负责人。
- [ ] 为 Worker Gateway 使用最小权限 PKI sign role。
- [ ] 演练 Vault token 丢失和恢复。
- [ ] 演练 old → dual trust → new CA 轮换。
- [ ] 演练旧 leaf、单 leaf 和整代 issuer 的吊销。
- [ ] 保存 Vault 配置备份及隔离恢复证据。

退出标准：控制面和 executor 不接触 CA 私钥；Vault 故障、恢复和 CA 轮换均不会
产生 fail-open worker identity。

### PostgreSQL、PBS 与 RPO/RTO

- [x] 恢复 Gate 从部署的 `portage-migrate supported-schema` 动态取得 authority，
  并在临时 PostgreSQL 18 上执行当前 migration/恢复 SQL；不再硬编码 v26。
- [ ] 在最终整合后的生产 schema v30（00027 → 00028 → 00029 → 00030）上配置 full、
  differential 和 WAL/PITR 备份。
- [ ] 将 backup/WAL repository 放到 NAS，并纳入 PBS VM 备份。
- [ ] 保持 PostgreSQL `PGDATA` 位于可靠本地块存储，不直接放在 NFS。
- [ ] 在隔离数据库中恢复并核验 schema、job、attempt、signing task、workload
  issuer、capacity action/instance、target outcome 和角色权限。
- [ ] 记录实际 RPO、RTO、备份大小、恢复耗时和负责人。

退出标准：不是只证明 dump 可以解压，而是能恢复当前 schema 的完整调度、身份、
签名和容量事实源。

### 对象存储容灾

- [x] 实现 read、quarantine、generation-GC、signer 四类最小权限输入检查，以及
  replication/rollback/deep-audit 的 fail-closed evidence 阶段。
- [ ] 为 API/read、executor/signer 和 lifecycle 使用独立最小权限身份。
- [ ] API/read 身份不具有 `DeleteObject`。
- [ ] quarantine delete 与 generation GC 使用不同权限边界。
- [ ] 配置跨故障域 replication。
- [ ] 验证 immutable generation、ETag CAS、channel rollback、deep audit 和
  reference-aware GC。
- [ ] 在主站不可用时，从副本恢复 stable pointer、manifest、Packages 和全部
  artifact。
- [ ] 记录对象存储 RPO/RTO 和恢复证据。

退出标准：丢失任一控制面副本或主对象存储后，已发布 binhost 仍可验证恢复，且
不会把半完成 generation 选为 stable。

### Signer 密钥生命周期

- [x] 实现离线加密备份、隔离恢复、双 key 验证、旧 generation/rollback 与私钥
  隔离检查的 Gate 契约。
- [ ] 建立签名私钥的离线加密备份。
- [ ] 在隔离环境演练 signer 恢复。
- [ ] 演练新旧 GPG key 轮换和双验证窗口。
- [ ] 验证 builder、API server 和 Dashboard 永远拿不到私钥。
- [ ] 验证旧 package、旧 generation 和 rollback 仍能按兼容策略校验。

退出标准：signer 丢失后可以在既定 RTO 内恢复；轮换过程中不会生成无法安装或
无法回滚的 binpkg generation。

### 公网入口与匿名页面

- [x] 提供 HTTPS Nginx/Compose reference edge，repository Gate 已验证 backend
  端口隔离、独立 source-IP/request/identity limit、cookie/CORS、metrics Basic
  Auth、匿名路由、frame-ancestors 拒绝，以及对 Dashboard 注册的每一条 WebShell
  路径的 404。WebShell 路径清单从 `internal/dashboard` 的路由源码推导，不再手写：
  手写清单曾漏掉 `/legacy/shell/` 与 `/api/shell/preflight`。
- [x] `/readyz` 只发布固定词表，不再回显内部错误文本。此前它把
  `databaseHealth.Reason` 与 `err.Error()` 写进匿名响应，pgx 的连接错误带有
  host、库名和账号，数据库不可达时即向任意调用者泄露内网拓扑；细节改为写进
  进程日志和需要鉴权的 `/api/v1/health`。仓库侧 Gate 由
  `internal/server/readyz_gate_test.go` 提供：行为侧断言响应体不含连接串，结构侧
  解析 `handleReadyz`/`handleLivez`/`refuseReadiness` 的 AST，拒绝任何非词表取值
  进入响应体。
- [ ] 部署 HTTPS edge/reverse proxy；API 和 Dashboard 可以继续在受限私网使用
  HTTP。
- [ ] 验证 Secure/SameSite cookie、HTTPS CORS allowlist 和 provider callback。
- [ ] 验证匿名 `/packages`、`/docs`、`/status`、`/binpkgs/.../Packages` 和 package
  下载。
- [ ] 验证 `/livez`、`/readyz` 仅返回最小信息。
- [ ] 验证 `/health` 需要 system-admin，metrics 需要 Basic Auth。edge 不重写
  `Authorization`，客户端的原始头会透传给控制面的 metrics 处理器，因此 edge
  htpasswd 的口令必须等于 `METRICS_PASSWORD`；这是同一个口令被校验两次，不是
  两个独立因子。要做成独立因子需要 edge 改用 `proxy_set_header Authorization`
  重写，属于单独一项变更。
- [ ] 验证 foreign-origin WebShell upgrade 被拒绝。
- [ ] Public Beta 阶段禁止从公网路由 WebShell；长期再实现短期 SSH certificate
  和 session recording。

退出标准：公开消费者无需账号即可搜索和下载包，但不能访问任务、日志、worker、
内部健康详情或任何管理接口。

## P2：监控与发布供应链增强

### 监控缺口

- [x] 增加 durable、固定 7 系列的低基数 lease expiry 指标和告警。
- [x] 增加 canonical terminal event-time Monitor projection lag 指标，并以
  PostgreSQL fresh/cached 边界回归验证 source/projected watermark 语义一致。
  不为该指标配告警：`deploy/observability/rules/portage-engine.yml` 与
  `docs/OBSERVABILITY_RUNBOOKS.md` 记录了原因——高于刷新间隔的阈值永远不触发，
  低于它的阈值会对正常刷新误报。投影不可用由
  `PortageEngineMonitorProjectionUnavailable` 覆盖。
- [ ] 在选定 billing export 后增加 provider invoice drift 指标。
- [x] 为 lease/projection 告警补充 runbook URL、owner、severity 和本地规则验证。

### Release pipeline

仓库 workflow 已完成，真实 registry promotion 尚未运行。

- [x] 选择 GHCR registry 和不可变 digest 命名规则。
- [x] 为全部生产角色定义 linux/amd64、linux/arm64 runtime image build/push。
- [x] 定义 OCI image、binary checksum 和 release manifest 的 keyless cosign 签名。
- [x] 建立 digest-bound candidate → stable promotion、CAS 与 rollback 流程。
- [x] 让 SPDX SBOM/provenance 可从发布制品独立获取和校验。
- [x] 固定基础镜像 digest、GitHub Actions SHA 与安全更新策略。
- [ ] 以受保护 GitHub environment 实际执行 candidate push/sign、stable promotion
  与 rollback；当前状态 **not-run**。

## P2：Distributed Build Alpha（仓库切片已实现；不阻塞 Public Beta）

distcc 应作为独立里程碑，不能直接打开全局开关。

- [x] 以 migration 00029 增加 compile worker inventory、原子 slot reservation、
  fenced lease 和 worker/builder heartbeat；00027/00028 分别保留给并行里程碑。
- [x] 按 architecture、CHOST、compiler/version digest、toolchain image generation、
  CPU feature、network zone 和 project trust domain 精确分池。
- [x] 仓库契约不承载 Portage repository、发布权限、签名私钥或 PVE/PBS 管理凭证；
  现场仍需审核实际 compile-worker image/mount/credential inventory。
- [x] lease contract 绑定 worker/builder/network identity/pool/expiry/fence；现场仍需
  用真实隔离网络和 mTLS/L4 sidecar 证明 `distccd` 只允许有效 lease。
- [x] 无条件禁用 pump mode。
- [x] server 与 builder 双重 reviewed C/C++ package allowlist。
- [x] 小包及 Rust、Go、Java 不进入 allowlist；Portage `package.env` 仅给 reviewed
  atom 启用编译 wrapper，configure、link、install、test 保持本地。
- [x] 持久化并导出低基数 local/remote compile、hit、fallback、network bytes、
  queue time 和固定 failure reason；builder compiler wrapper 已把真实调用聚合回
  manager 的 `RecordCompileObservation` 调用链，现场 sidecar 仍须提供网络授权证据。
- [x] 增加 local-only 与 distcc artifact digest、ABI、安装和 GUI evidence 的
  fail-closed 仓库 Gate `make distcc-gate`。
- [x] 断开时实现 `local` fallback 或 `blocked`；失败 emerge 不进入 collection；
  output fence 在 collection 前及下载后/staging commit 前复验，失败清理隔离结果。
- [ ] 以真实 distccd/PVE、至少两个并行 job 和 worker disconnect 完成现场 Gate，
  保存数据库、网络策略、日志、metrics 与对照 receipt；当前状态 **not-run**，仓库
  不伪造现场结果。

退出标准：至少两个 job 并行，reviewed C/C++ workload 能借用同构 compile pool；
资源不超卖，断开 worker 可控降级，产物仍通过同一签名、安装和发布 Gate。

## P2：用户体验与 GUI E2E 扩展

- [x] 为 CLI 增加 browser-assisted/device authorization flow；device code 只持久化
  hash，并覆盖 pending/slow-down/deny/expiry/concurrent one-time consumption。
- [x] 增加 GTK Mousepad 的安装、启动、fixture、断言和退出场景。
- [x] 增加 Qt FeatherPad 场景。
- [x] 增加 WebKitGTK surf 的 WebView 场景。
- [ ] 注入真实签名 candidate digest/fingerprint，并在 PVE 执行完整 matrix；tracked
  sentinel 会 fail closed，当前状态 **not-run**。
- [ ] 基线稳定后再扩展 KDE/GNOME 模板。
- [x] 视觉 AI 只用于失败 triage 和候选 selector/needle 建议，不作为 release
  oracle。

## P3：Provider invoice reconciliation

在没有真实账单输入契约前，不实现所谓“通用账单 importer”。

- [ ] 选择实际 provider billing export 格式和交付方式。
- [ ] 定义 provider resource identity 与 capacity instance、attempt、project 的关联。
- [ ] 保留原始账单对象摘要和 ingestion watermark。
- [ ] 实现幂等导入、重复账单拒绝和更正记录。
- [ ] 对比 admission estimate、runtime settlement 和 provider invoice。
- [ ] 将偏差输出到 Monitor、审计和低基数告警。

## P4：GA 运行 Gate

- [ ] 连续运行 30 天。
- [ ] 保存 availability、queue age、target SLO、失败分类和告警证据。
- [ ] 完成一次控制面副本故障演练。
- [ ] 完成一次 PostgreSQL PITR 演练。
- [ ] 完成一次对象存储主站故障恢复演练。
- [ ] 完成一次 Vault/worker CA 恢复和轮换演练。
- [ ] 完成一次 signer key 恢复或轮换演练。
- [ ] 完成社区安全 review。
- [ ] 清零未处理 P0/P1。

## 推荐执行顺序

1. 审阅、合并并推送 `codex/next-steps-integration`，确认 GitHub CI/CodeQL/安全扫描。
2. 用真实 PVE 完成 persistent-executor SCHED-2B 与签名 GUI matrix Gate。
3. 部署正式 HTTPS edge，完成 Authentik、Google、GitHub 回调与 real-host Gate。
4. 完成 Vault HA、schema v30 PostgreSQL/PBS、对象存储和 signer 现场恢复演练。
5. 以受保护 GitHub environment 执行真实 candidate/stable/rollback 发布。
6. Public Beta 不依赖 distcc；有隔离网络后再执行真实 distccd 双 job/disconnect Gate。
7. 选择实际 billing export 后实现 invoice reconciliation，随后开始 30 天稳定窗口。

## 相关文档

- [企业内部构建中心差距分析](ENTERPRISE_GAPS.md)
- [设计决策与被否决的方案](DESIGN_DECISIONS.md)
- [生产部署边界](PRODUCTION_BOUNDARY.md)
- [对象存储契约](OBJECT_STORAGE.md)
- [调度与容量](SCHEDULER.md)
- [监控告警与演练](OBSERVABILITY_RUNBOOKS.md)
- [身份提供方](IDENTITY_PROVIDERS.md)
- [Public Beta 恢复 Gate](PUBLIC_BETA_RECOVERY.md)
- [Distributed Build Alpha](DISTRIBUTED_BUILD_ALPHA.md)
- [PVE 验证](PVE_TESTING.md)
- [桌面 E2E](DESKTOP_E2E.md)
