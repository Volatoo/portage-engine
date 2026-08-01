# Portage Engine 后续待办与验收计划

更新日期：2026-08-01

## 当前结论

仓库内的核心控制面、对象存储发布链路、独立签名、匿名公共页面、
PostgreSQL/Redis 状态平台、PVE 一次性 builder、镜像工厂和基础 GUI E2E
已经形成闭环。下一阶段的重点不是继续扩展控制面功能，而是完成 Public
Beta 所需的真实生产环境 Gate，并保存可审计的恢复和运行证据。

当前 `main` 比 `origin/main` 领先一个提交：

```text
f79fca4 feat: complete public artifact and community surfaces
```

## P0：仓库收口

- [ ] 检查本地 `.playwright-mcp/` 浏览器测试输出；确认无需保留后删除，或加入
  本地 exclude，避免测试日志进入版本控制。
- [ ] 将 `f79fca4` 推送到 `origin/main`。
- [ ] 统一路线图中的调度边界描述：v1 以 project 作为公平和配额边界，
  capacity pool 负责 hard routing 与容量隔离，不默认实现 target/provider
  层级公平子队列。
- [ ] 推送后确认 GitHub CI、CodeQL 和安全扫描全部通过。

退出标准：工作区干净，远端 `main` 包含当前提交，CI 全绿，路线图不存在与
当前实现相矛盾的调度描述。

## P1：Persistent Executor 与真实 SCHED-2B Gate

这是推荐最先实施的里程碑。调度器、capacity action/instance 账本、provider
actuator、lease/fence、drain 和删除保护已经实现，目前缺少专用 PVE 模板与现场
验收。

- [ ] 构建独立的 persistent-executor Gentoo 模板，不能复用一次性 job builder。
- [ ] 模板只包含 executor 所需组件，不包含签名私钥、PVE/PBS 管理凭证或 API
  listener credential。
- [ ] 通过 SMBIOS/instance metadata 注入数据库生成的 capacity instance ID。
- [ ] 启动后主动连接 Worker Gateway，注册精确的 provider、zone、architecture、
  build mode、profile 和 image generation capability。
- [ ] 验证 actuator 的 create action 只能生成一个精确实例。
- [ ] 验证重复执行、stale fence 和超时重试不会生成重复 VM。
- [ ] 验证 scale-down 首先进入 draining。
- [ ] 在 admission lease 或 phase lease 存活时，删除必须 fail closed。
- [ ] 所有 lease 归零后，只按 provider instance ID、owner generation 和 exact
  VM identity 删除。
- [ ] 通过 PVE API readback 确认 VM 确实不存在。

退出标准：完成一次真实的 scale-up → heartbeat → 执行任务 → drain → delete；
同时完成“有 live work 时拒绝删除”的负向 Gate，并保存数据库、PVE API、日志和
监控证据。

## P1：Public Beta 生产环境 Gate

### 真实身份提供方

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

- [ ] 部署 Vault HA，并明确 storage、unseal 和恢复负责人。
- [ ] 为 Worker Gateway 使用最小权限 PKI sign role。
- [ ] 演练 Vault token 丢失和恢复。
- [ ] 演练 old → dual trust → new CA 轮换。
- [ ] 演练旧 leaf、单 leaf 和整代 issuer 的吊销。
- [ ] 保存 Vault 配置备份及隔离恢复证据。

退出标准：控制面和 executor 不接触 CA 私钥；Vault 故障、恢复和 CA 轮换均不会
产生 fail-open worker identity。

### PostgreSQL、PBS 与 RPO/RTO

- [ ] 在生产 schema v26 上配置 full、differential 和 WAL/PITR 备份。
- [ ] 将 backup/WAL repository 放到 NAS，并纳入 PBS VM 备份。
- [ ] 保持 PostgreSQL `PGDATA` 位于可靠本地块存储，不直接放在 NFS。
- [ ] 在隔离数据库中恢复并核验 schema、job、attempt、signing task、workload
  issuer、capacity action/instance、target outcome 和角色权限。
- [ ] 记录实际 RPO、RTO、备份大小、恢复耗时和负责人。

退出标准：不是只证明 dump 可以解压，而是能恢复当前 schema 的完整调度、身份、
签名和容量事实源。

### 对象存储容灾

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

- [ ] 建立签名私钥的离线加密备份。
- [ ] 在隔离环境演练 signer 恢复。
- [ ] 演练新旧 GPG key 轮换和双验证窗口。
- [ ] 验证 builder、API server 和 Dashboard 永远拿不到私钥。
- [ ] 验证旧 package、旧 generation 和 rollback 仍能按兼容策略校验。

退出标准：signer 丢失后可以在既定 RTO 内恢复；轮换过程中不会生成无法安装或
无法回滚的 binpkg generation。

### 公网入口与匿名页面

- [ ] 部署 HTTPS edge/reverse proxy；API 和 Dashboard 可以继续在受限私网使用
  HTTP。
- [ ] 阻止外部直接访问 backend HTTP 端口。
- [ ] 配置独立的 edge source-IP/request-rate limit；Redis 不是公网 DoS 边界。
- [ ] 验证 Secure/SameSite cookie、HTTPS CORS allowlist 和 provider callback。
- [ ] 验证匿名 `/packages`、`/docs`、`/status`、`/binpkgs/.../Packages` 和 package
  下载。
- [ ] 验证 `/livez`、`/readyz` 仅返回最小信息。
- [ ] 验证 `/health` 需要 system-admin，metrics 需要独立 Basic Auth。
- [ ] 验证 foreign-origin WebShell upgrade 被拒绝。
- [ ] Public Beta 阶段禁止从公网路由 WebShell；长期再实现短期 SSH certificate
  和 session recording。

退出标准：公开消费者无需账号即可搜索和下载包，但不能访问任务、日志、worker、
内部健康详情或任何管理接口。

## P2：监控与发布供应链增强

### 监控缺口

- [ ] 增加低基数 lease expiry 指标和告警。
- [ ] 增加 Monitor projection lag 指标和告警。
- [ ] 在选定 billing export 后增加 provider invoice drift 指标。
- [ ] 为每条告警补充 runbook URL、owner、severity 和演练记录。

### Release pipeline

当前 CI 已生成 runtime SPDX SBOM、BuildKit provenance 和 SHA-256 checksum，但
还没有完整的社区 release promotion。

- [ ] 选择 OCI registry 和命名规则。
- [ ] 推送按角色拆分的 runtime images。
- [ ] 对 OCI image、binary checksum 和 release manifest 签名。
- [ ] 建立 candidate → stable promotion 和 rollback 流程。
- [ ] 验证 SBOM/provenance 可以从发布制品独立获取和校验。
- [ ] 建立基础镜像 digest 与安全更新策略。

## P2：Distributed Build Alpha（不阻塞 Public Beta）

distcc 应作为独立里程碑，不能直接打开全局开关。

- [ ] 增加 compile worker inventory、slot reservation、lease 和 heartbeat。
- [ ] 按 architecture、CHOST、compiler/version digest、toolchain image generation、
  CPU feature、network zone 和 project trust domain 精确分池。
- [ ] compile worker 不持有 Portage repository、发布权限、签名私钥或 PVE/PBS
  管理凭证。
- [ ] `distccd` 只监听隔离构建网络，并限制到持有有效 lease 的 builder。
- [ ] 默认禁用 pump mode。
- [ ] 只对 reviewed C/C++ package allowlist 启用。
- [ ] 小包及 Rust、Go、Java、configure、link、test 保持本地执行。
- [ ] 收集 local/remote compile、hit rate、fallback、network bytes、queue time 和
  failure reason。
- [ ] 对比纯本地与 distcc 的 artifact digest、ABI、安装和 GUI 结果。
- [ ] 断开 compile worker 时必须安全 fallback 或 blocked，不能污染 staging。

退出标准：至少两个 job 并行，reviewed C/C++ workload 能借用同构 compile pool；
资源不超卖，断开 worker 可控降级，产物仍通过同一签名、安装和发布 Gate。

## P2：用户体验与 GUI E2E 扩展

- [ ] 为 CLI 增加 browser-assisted/device authorization flow，减少手工复制 token。
- [ ] 增加一个 GTK 应用的安装、启动、fixture、断言和退出场景。
- [ ] 增加一个 Qt 应用场景。
- [ ] 增加一个 Electron/WebView 应用场景。
- [ ] 基线稳定后再扩展 KDE/GNOME 模板。
- [ ] 视觉 AI 只用于失败 triage 和候选 selector/needle 建议，不作为 release
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

1. 仓库收口并推送当前提交。
2. 构建 persistent-executor 模板并完成真实 PVE SCHED-2B Gate。
3. 部署正式 HTTPS edge，并完成 Authentik、Google、GitHub 回调。
4. 完成 Vault HA、PostgreSQL/PBS、对象存储和 signer 恢复演练。
5. 补齐 lease/projection 告警与 release promotion/signing。
6. 开始 30 天稳定性窗口。
7. Public Beta 不依赖 distcc；distcc、CLI device flow 和扩展 GUI 矩阵并行作为
   后续独立里程碑。

## 相关文档

- [总体路线图](ROADMAP_AND_DESKTOP_E2E.html)
- [生产部署边界](PRODUCTION_BOUNDARY.md)
- [对象存储契约](OBJECT_STORAGE.md)
- [调度与容量](SCHEDULER.md)
- [身份提供方](IDENTITY_PROVIDERS.md)
- [PVE 验证](PVE_TESTING.md)
- [桌面 E2E](DESKTOP_E2E.md)
