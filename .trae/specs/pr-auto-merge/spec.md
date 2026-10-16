# 拉取请求自动合并（Pull Request Auto-Merge）- 产品需求文档

## Overview

- **Summary**: 为仍处于开放状态的拉取请求增加"自动合并意图"（auto-merge intent）：维护者可在拉取请求页面、owner JSON API 和 `owngit pr` 命令行启用、查看或取消该意图。后台收敛循环仅依据该请求**当前源提交**的检查投影与当前提交对的 Git 可合并性决定何时发布合并；证据不满足时呈现明确的等待/阻塞状态且绝不发布合并。
- **Purpose**: 检查跑完时维护者不必守在页面前手工点击合并；同时不改变"检查仅供参考、手工合并永不被检查拦截"的既有产品原则——自动合并是一种每请求显式选择加入（opt-in）的意图。
- **Target Users**: OwnGit 安装的所有者/维护者（浏览器仪表盘使用者），以及通过 owner API 与 `owngit pr` 操作拉取请求的编码工具与脚本。

## Goals

- 开放请求可在浏览器页面、owner API、CLI 三个面上启用自动合并，并随时查看意图状态、取消意图。
- 自动合并的唯一放行条件是：意图仍有效 + 请求开放 + 当前源提交的检查投影终态通过且确实测试当前提交、投影新鲜（未过期）、证据可读取 + 当前源/目标提交对 Git 可干净合并。
- 源分支推进后意图保留，但旧成功不得复用；目标分支移动后重新核对当前提交对与 Git 可合并性。
- 检查完成、源/目标 ref 变化、仓库就绪/恢复等事件唤醒收敛；重复唤醒具有合并性，合并回执（现有 receipt 格式）至多产生一次。
- 取消/关闭与后台收敛并发时具有确定次序：先提交的取消或关闭阻止发布；已发布的合并按回执恢复为 merged，不误报取消、不二次合并。
- 待处理意图跨服务重启与备份恢复保留；仓库暂不可用或冲突时保持可重试，后续事件能继续收敛。
- 未启用自动合并的请求完全沿用现状：检查仅供参考的手工合并语义，现有关闭、重开、检查展示与合并接口保持兼容。

## Non-Goals

- 不把评审（review）结论作为自动合并门控：评审继续仅供参考（已与需求方确认）。
- 不改变手工 `Merge` 路径的任何放行规则；不新增强制检查策略、分支保护规则或必需检查名单。
- 不为自动合并新增外部通知渠道（邮件/IM/webhook）；合并结果沿用现有合并记录、receipt ref 与页面/托盘既有呈现。
- 不做"变基后自动合并""合并队列/串行化队列""合并方法选择（squash/rebase）"等新合并模式；自动合并沿用现有 fast-forward / merge-commit / up-to-date 机制。
- 不改变检查的调度、执行与证据模型本身；自动合并只读取既有检查投影。

## Background & Context

既有机制（本特性复用而非重建）：

- `internal/pullrequest/service.go`：`Service.Merge`（L383-539）已在仓库写锁内完成"读取当前头→校验期望修订→计划合并→保护对象→`git update-ref --stdin` 事务发布 receipt→对账"的完整幂等流程；`reconcileIntentLocked`（L801-817）、`validateReceipt`（L819-851）、`completePublishedMergeLocked`（L611-629）保证"Git 已发布但状态未记完"的合并被任何后续调用者恰好补登一次。`Close` 路径已先调用 `completePublishedMergeLocked` 防止已发布合并被记为关闭。
- 合并计划/回执表 `pull_request_merge_intents`（状态 preparing/planned/ready/complete）与受保护 ref `refs/owngit/pull-requests/<n>/merge-receipt` 共同提供去重与崩溃恢复权威；receipt 事务在 `git.go` `publishMerge`（L504-522）中以 `verify` 头修订 + 同一 `update-ref` 事务创建。
- 检查投影：`checksForRevision`（service.go L1001-1058）已按**当前源 OID** 计算 `status / stale / tested_commit / read_failure / configuration_version`；`LatestCheckAttemptForRevision` 按 sequence 取最新 attempt（旧结果不覆盖新状态）；`checksFromAttempt`（L1060-1088）的 `TestedCommit` 要求同修订 + `EffectiveWorktreeState()==clean` + `!CleanupFailed()`；配置新鲜度比较源提交内提交的 `.owngit/checks.json` 与 attempt 使用的配置版本。
- 唤醒模式：`checkrun.Coordinator` 使用 cap-1 合并 channel（`Wake` L242-253）+ 定时兜底；`cmd/owngit/main.go` 中 `noteWrite`（L536-539）已汇聚推送（`gitHandler.OnReceive/OnPush` L749-760）、仓库就绪/恢复/导入与 `pullRequests.OnChange`（L809）。检查终态写入点有三处：本地 worker `executeLocal`（coordinator.go L1151 `CompleteCheckJobAttempt`，随后 L1034 `Wake`）、runner HTTP 完成端点（`internal/server/configured_checks.go` L454）、helper HTTP 完成端点（`internal/server/checks.go` L195 `CompleteCheckAttempt`）；后两者目前不唤醒任何合并逻辑。
- 持久化：SQLite schema 当前版本 17（`internal/state/store.go` schemaSteps），新表追加 step 18。可移植表须接入备份恢复四触点（`state/recovery.go` 的 RecoveryState、`readPullRequestRecovery`、`ValidatePullRequestRecovery`、`restorePullRequestRecovery` 与删除顺序表）及 `internal/recovery/recovery.go` 清单（当前 backupVersion=12，含无新记录时降级写旧格式的 content-gate 机制）。
- 接口面：owner JSON API 为手工分发器（`internal/server/api.go`，路由文法 L406-439），PR 基址 `/api/v1/repositories/{r}/pull-requests`；浏览器为服务端渲染表单 + CSRF（`internal/server/pull_requests_browser.go`、`repositories.go` PR 分支 L376-418）；CLI `cmd/owngit/pr.go` 为 stdlib flag 子命令开关（L38-59），输出原样透传服务端 JSON，端点构造在 `cmd/owngit/operations.go`。

## Functional Requirements

### 意图管理

- **FR-1（启用）**：对开放请求可启用自动合并。启用是按请求（而非按修订）的持久意图：不绑定某次源/目标 OID，后续当前提交对变化时意图持续适用。重复启用是幂等的（返回同一意图与当前状态，不产生新记录或错误）。对 closed/merged 请求启用被拒绝。
- **FR-2（查看）**：意图状态随拉取请求视图（show；list 行以受限形式）返回：是否启用、评估的提交对、等待或阻塞原因码与可读文案、启用时间/启用者。未启用时该字段缺省（omitempty），现有人工合并视图不被打扰。
- **FR-3（取消）**：可随时取消一个存在的意图。取消不存在的意图返回明确问题码（非成功）。合并已发布的竞态按 FR-9 处理，绝不返回"已取消"。
- **FR-4（关闭即撤销）**：手动关闭开放请求时，若启用了自动合并，意图在关闭同一事务中被撤销；重开不会自动恢复意图，需重新启用（已与需求方确认）。

### 自动放行判定

- **FR-5（证据门控）**：收敛只针对意图有效且状态为 open 的请求，读取其**当前源分支头**对应检查投影。仅当以下全部成立时证据门控通过：
  1. 当前源提交存在最新 attempt 且为**终态通过**（`passed`；pending 非终态）；
  2. `tested_commit=true`：attempt 修订等于当前源 OID、有效工作树状态为 clean、清理无失败（dirty/unknown/cleanup_failed 均不通过）；
  3. 投影**新鲜（未过期）**：不处于 stale——证据配置版本与当前源提交中提交的工作流检查定义一致（无提交工作流文件的助手证据沿用现有规则视为不 stale）；取该修订最新一次终态 attempt；原始日志保留期不作为门控（已与需求方确认）；
  4. 证据与配置**可读取**（无 read_failure）。
- **FR-6（等待态）**：下列情况呈现明确"等待"状态且不发布合并，并在后续事件到来时重试：证据缺失（当前提交无任何 attempt；或仅有旧修订证据）→ waiting_for_checks；attempt 运行中（pending）→ checks_running；证据/配置读取失败 → evidence_unreadable；Git 合并冲突或无共同合并历史 → merge_conflict；仓库忙、暂不可用或状态存储错误 → repository_unavailable。
- **FR-7（阻塞态）**：下列情况呈现明确"阻塞"状态且不发布合并：当前提交最新终态 attempt 为非通过终态（failed/error/cancelled/incomplete/unavailable）→ checks_failed；通过但工作树不干净/未知或清理失败 → checks_untrusted_worktree；通过但检查配置已变化（stale）→ checks_configuration_changed；源/目标分支缺失或不指向提交 → 复用现有 `*_branch_missing`/`*_not_commit` 阻塞码。阻塞针对当前修订：同一修订出现新的通过证据（更新 sequence）可解除；源分支推进后按 FR-8 重新评估。
- **FR-8（修订推进）**：源分支推进后意图保留；投影改按新源 OID 计算，旧提交的通过结果在新 pair 上呈现为 waiting_for_checks（stale 展示），绝不作为放行依据。目标分支移动后重新读取当前目标头、重新计算合并方法与三方合并试验；旧合并计划不得发布（沿用既有 superseded plan 丢弃与 head `verify` 机制）。
- **FR-9（合并发布与幂等）**：门控全部通过时，收敛在同一把仓库写锁持有期内复用现有 merge-intent 机制（plan→保护对象→`update-ref` 事务+receipt→对账完成）发布合并；发布前在锁内再次确认意图行仍存在且请求仍开放，否则放弃发布。合并产生的 MergeResult、receipt ref、merged 记录与人工合并格式完全一致；任意次数的重复唤醒/重试至多产生一次合并与一份回执。自动合并记入 `merged_by`（启用者 actor，缺失时为空 actor，与现有恢复路径一致）。
- **FR-10（无证据默认等待）**：仓库未配置检查、当前源提交也无任何证据时，启用意图后持续 waiting_for_checks，不自动合并（已与需求方确认）；助手或任务之后为该提交提交合格通过证据时即收敛合并。

### 唤醒与收敛

- **FR-11（唤醒源）**：以下事件唤醒收敛：仓库收到推送（源/目标或任意分支 ref 变化）、PR 创建/合并/关闭等既有 OnChange、仓库就绪/备份恢复/导入发布；以及三类检查终态写入：本地 worker 完成、runner HTTP complete/unavailable、helper HTTP attempt 完成。启动恢复完成后执行一次全量收敛。
- **FR-12（合并循环）**：后台收敛器使用 cap-1 合并信号（同信号多次触发只导致一次待处理扫描）+ 粗间隔定时兜底；每次扫描只查询存在启用意图的仓库/请求，逐仓库在写锁内有界、公平地处理；不在持锁路径之外产生 Git 写效果。冲突/不可用的请求不做忙等重试，等待下一个事件或兜底 tick。进程关闭时收敛循环可取消并被等待退出。

### 持久化与恢复

- **FR-13（schema 与备份）**：自动合并意图持久化于新表（schema 追加迁移步骤，不改动既有步骤），并纳入可移植备份：备份写出、恢复写入、备份/恢复两侧校验、恢复目标删除顺序、备份清单字段与备份格式版本提升（无该类记录的备份仍按旧格式写出，沿用既有 content-gate 降级规则）。校验不变量：每条启用意图对应一个存在且 open 的拉取请求；merged/closed 请求不存在意图行。
- **FR-14（重启与恢复后收敛）**：服务重启后意图仍在；仓库准备恢复完成后被唤醒并可继续合并。备份恢复到新实例后意图保留，后续检查完成/ref 变化事件能够继续收敛；恢复时尚未发布的旧合并计划沿用现有 merge-intent 恢复规则。

### 兼容性

- **FR-15（既有行为不变）**：未启用自动合并的请求，其创建、评审、关闭、重开、diff、mergeability、手工 merge 行为与响应完全不变；检查与评审继续标注 advisory，绝不进入手工合并阻塞项。JSON 变更仅为新增字段（缺省省略），既有字段语义、错误码、HTTP 状态映射保持。
- **FR-16（三端一致与可访问性）**：浏览器页面在开放请求上提供启用/取消控件与等待/阻塞状态呈现；CLI 提供启用/取消子命令并透传 JSON 视图；owner API 提供启用/取消端点，鉴权与请求体规则（JSON-only、Basic/开放模式、CSRF 表单）与现有 PR 变更端点一致。状态不可仅用颜色表达，键盘可达，Light/Dark/System 外观可用；英文默认、韩文同步。

## Non-Functional Requirements

- **NFR-1（并发正确）**：取消/关闭与后台发布之间的线性次序由"每仓库写锁 + 意图行在状态翻转同一 SQLite 事务中删除 + Git receipt 对账"三者共同保证；不得依赖 goroutine 时序或 sleep 同步。需有可重复的确定性竞态测试覆盖两种次序。
- **NFR-2（成本有界）**：列表视图与无意图仓库的唤醒扫描不得运行 merge-tree 试合并；每次扫描的仓库数、每仓库请求数有界并可公平推进；Git 进程数量不因启用意图的请求数而失控（等待态不轮询 Git，由事件驱动，定时兜底间隔为分钟级）。
- **NFR-3（可观测失败）**：所有等待/阻塞原因均有稳定代码与面向维护者的可读文案（API JSON、CLI 原样 JSON、浏览器本地化文案），读取失败与仓库不可用不得被呈现为"无检查/可合并"。
- **NFR-4（安全与隐私）**：新增端点、输出与日志不引入凭据/令牌/私有仓库内容；自动合并不授予任何新的访问权，执行者身份沿用 general access 既有 actor 模型。
- **NFR-5（可测试性）**：核心判定（投影→原因码、锁内发布、取消竞态、superseded 计划丢弃、重启/恢复保留）在 `internal/pullrequest`、`internal/state`、`internal/server`、`cmd/owngit` 与 `internal/recovery` 包内有聚焦的 Go 测试；不依赖外网与真实 sleep。

## Constraints

- **Technical**: Go 标准库与既有依赖（modernc sqlite 等），不新增第三方依赖；遵循 `docs/DEVELOPMENT.md`：schema 迁移只追加步骤；备份格式升版并保持旧版可读；新公共文档需英文+韩文同步并更新 CHANGELOG；PR 相关 Git 命令继续受仓库锁与受保护 ref 约束；JSON 经 bidi 转义。
- **Business**: 单一共享密码/开放访问模式，无用户账号；自动合并代表所有者自身的操作，不引入审批流。
- **Dependencies**: 现有 pull-request merge-intent/receipt 机制、检查 attempt 投影、checkrun 协调器唤醒、SQLite 迁移与备份恢复管线。

## Assumptions

- "自动合并只依据检查投影与 Git 可合并性"，评审结论不门控（需求方确认）。
- 合并方法与人工合并相同（fast_forward/merge_commit/up_to_date），不新增合并选项。
- 三个终态写入点（本地 worker、runner、helper HTTP）覆盖所有检查证据变终态的途径；Actions 计划任务等其他来源最终也经由这些 attempt/job 终态写入点落库。
- 意图不记录目标修订：取消/关闭前，意图永远表示"当当前 pair 满足条件时合并"。

## Acceptance Criteria

### AC-1: 三端启用与幂等
- **Type**: `rule`
- **Given**: 仓库中存在一个 open 拉取请求。
- **When**: 维护者分别通过浏览器 PR 页面表单、`POST /api/v1/repositories/{r}/pull-requests/{n}/auto-merge`（`{}` 体）、`owngit pr auto-merge --number N` 启用自动合并，并对同一请求再次启用。
- **Then**: 首次启用持久化意图并返回含 `auto_merge.enabled=true` 与当前状态/启用时间的视图；再次启用幂等返回同一意图（不新增行、不报错、不触发合并）；closed/merged 请求启用返回 4xx 问题码且不写意图。
- **Pass Condition**: 三端各有测试证明首次启用成功、重复启用幂等、非 open 请求被拒；意图行恰有一条。
- **Evidence**: `internal/server`（API 与浏览器处理器）测试、`cmd/owngit/pr_test.go` 风格 CLI 测试、`internal/state` 存储测试。

### AC-2: 查看意图状态（show 完整、list 受限）
- **Type**: `rule`
- **Given**: 一个启用自动合并的 open 请求。
- **When**: 调用 show 端点 / `owngit pr show` / 浏览器打开 PR 页，以及 list 端点与列表页。
- **Then**: show 与页面返回启用标记、评估的当前 source/target OID、状态（waiting 或 blocked）、稳定原因码与文案、启用时间/者；list 行返回启用标记与**不依赖试合并**的原因（证据门控未过时不得伪装成可合并，试合并级结论仅在 show 计算）。
- **Pass Condition**: 测试断言 show 含完整原因码；list 路径不调用 merge-tree 试合并（代码审查 + 无试合并副作用断言），未启用请求的 JSON 中 `auto_merge` 字段缺省。
- **Evidence**: service 投影单测、server API 测试、浏览器行为测试。

### AC-3: 取消意图
- **Type**: `rule`
- **Given**: open 请求已启用自动合并。
- **When**: 通过三端之一取消（`.../auto-merge/cancel`、`owngit pr auto-merge --number N --cancel`、页面取消表单），随后再次取消，并观察后续检查完成/推送事件。
- **Then**: 首次取消删除意图并返回无 `auto_merge` 的视图；再次取消返回意图不存在的问题码；取消后任何唤醒都不为该请求发布合并；未发布的旧合并计划被安全丢弃。
- **Pass Condition**: 取消成功、重复取消有明确错误、取消后事件到达也不合并，三者均有测试。
- **Evidence**: service/状态机测试、API/CLI 测试。

### AC-4: 合格证据出现即无人值守合并
- **Type**: `rule`
- **Given**: open 请求已启用自动合并，当前源提交证据尚缺失（waiting_for_checks）。
- **When**: 该源提交出现终态 passed、干净工作树、配置一致、可读取的检查证据（经 helper、runner 或本地 worker 任一途径）。
- **Then**: 终态写入触发的一次收敛内，目标分支在无人进一步操作的情况下被更新为合并结果；PR 变为 merged，MergeResult 模式/OID/receipt_ref/merged_at 与人工合并格式一致；`merged_by` 反映启用者 actor。
- **Pass Condition**: 端到端测试证明三条证据途径（至少 helper HTTP 与协调器本地途径各一）写入后无需任何手工调用即合并，且回执字段格式与人工合并用例一致。
- **Evidence**: `internal/pullrequest` + `internal/checkrun`/`internal/server` 集成测试。

### AC-5: 缺失/运行中/读取失败只等待不合并
- **Type**: `rule`
- **Given**: 已启用意图的 open 请求。
- **When**: 当前源提交分别处于：无任何 attempt；仅有旧源提交的 attempt（stale）；最新 attempt 为 pending；检查配置或证据表读取失败（注入 Store 错误）。
- **Then**: 视图与 API 分别给出 waiting_for_checks / waiting_for_checks（stale 展示）/ checks_running / evidence_unreadable；任何唤醒/定时收敛都不发布合并；状态恢复可读或终态证据到达后可继续合并。
- **Pass Condition**: 四种情形各有单测断言原因码且断言无合并发布（目标 ref 不变、无 receipt ref）；恢复后一次唤醒完成合并。
- **Evidence**: `internal/pullrequest` 投影与收敛测试。

### AC-6: 失败/脏工作树/配置变化阻塞当前修订
- **Type**: `rule`
- **Given**: 已启用意图的 open 请求，当前源提交存在证据。
- **When**: 最新终态 attempt 分别为 failed（含 error/cancelled/incomplete/unavailable）；passed 但 effective worktree 为 dirty/unknown 或 cleanup_failed；passed/tested 但其配置版本与源提交提交的工作流定义不一致。
- **Then**: 分别呈现 checks_failed / checks_untrusted_worktree / checks_configuration_changed 阻塞态且不合并；同一修订随后出现更新 sequence 的合格通过证据时阻塞解除并合并；不发生任何合并写效果。
- **Pass Condition**: 三类阻塞各有测试；同修订新通过证据（sequence 更大）解除阻塞有测试。
- **Evidence**: service 投影单测（构造 state.CheckAttempt 夹具）。

### AC-7: 源/目标修订移动后重新核对
- **Type**: `rule`
- **Given**: 已启用意图，旧源提交曾有通过证据。
- **When**: 源分支被推到新提交；或目标分支移动；或两者同时移动。
- **Then**: 意图保留；投影按新源 OID 计算，旧成功不被复用（waiting_for_checks 直至新提交有合格证据）；目标移动后按新 pair 重新判定合并方法与冲突：冲突为 waiting(merge_conflict) 不发布，目标再次移动消除冲突后由 ref 变化唤醒完成合并；整个过程无针对旧 pair 的发布。
- **Pass Condition**: 测试覆盖源推进、目标推进致冲突、目标再推进解冲突三个序列，断言旧 pair 无 receipt、最终合并发生在最新 pair。
- **Evidence**: `internal/pullrequest` 收敛序列测试（真实 git 夹具）。

### AC-8: 事件唤醒且重复通知只有一份回执
- **Type**: `rule`
- **Given**: 已启用意图且门控全部满足。
- **When**: 短时间内重复注入多次检查完成通知与多次 ref 变化通知（合并 channel 场景）。
- **Then**: 目标分支只更新一次，`refs/owngit/pull-requests/<n>/merge-receipt` 与 merge_intent complete 记录恰一份；后续重复唤醒不再产生合并提交或第二份回执。
- **Pass Condition**: 并发/重复唤醒测试断言目标 OID、receipt、merge_intent 行数与状态。
- **Evidence**: 收敛器并发测试 + 既有 receipt 归属校验。

### AC-9: 取消/关闭与发布并发的确定次序
- **Type**: `rule`
- **Given**: 已启用意图、门控满足，后台收敛与取消/关闭操作并发。
- **When**: 次序 A：取消（或关闭）先于 Git 发布获得写锁并提交；次序 B：Git receipt 已在取消/关闭获锁前发布。
- **Then**: 次序 A 中收敛在锁内复查意图缺失即放弃，绝不发布合并，请求保持 open（或 closed）；次序 B 中取消/关闭按 receipt 把合并补登为 merged，返回 merged 结果（复用既有 `pull_request_merged` 语义），绝不报"已取消"，且不二次合并。两种次序各重复执行结果确定。
- **Pass Condition**: 用可注入的发布栅栏分别强制两种次序的测试通过；不存在"关闭后又变 merged 之外的状态""取消成功但请求仍合并"等交错。
- **Evidence**: `internal/pullrequest` 竞态测试（参考 close_test.go 既有 completePublishedMerge 测试手法）。

### AC-10: 冲突与仓库不可用保持可重试
- **Type**: `rule`
- **Given**: 已启用意图。
- **When**: 当前 pair 三方合并冲突（或无合并历史）；或仓库存储暂时不可用/忙（Git 命令失败、锁占用）。
- **Then**: 状态为 waiting(merge_conflict) 或 waiting(repository_unavailable)，意图保留、无错误升级、无忙等；此后一次目标 ref 变化或检查完成事件（仓库恢复后的就绪事件）即重新收敛，条件满足时合并成功。
- **Pass Condition**: 冲突→等待→目标推进→合并；不可用→等待→恢复→合并，两条序列测试通过；无意图请求在这些事件下不产生额外 Git 活动。
- **Evidence**: service 测试（故障注入）与收敛器重试测试。

### AC-11: 意图跨重启与备份恢复保留
- **Type**: `rule`
- **Given**: 一个存在启用意图（处于等待态）的实例。
- **When**: 场景一重启服务进程；场景二执行备份并在干净实例恢复后启动；场景三在合并计划已生成但未发布时重启。
- **Then**: 场景一意图仍在，恢复完成后被唤醒并可在证据满足时合并；场景二备份与恢复后的库中意图行完整保留且通过备份校验，恢复后事件可继续收敛；场景三沿用既有未完成 merge-intent 恢复规则，不重复发布、不丢失已发布结果。备份校验拒绝：意图指向不存在/非 open 请求、closed/merged 请求携带意图行等非法清单。
- **Pass Condition**: state 迁移测试（旧版本库升级到新 schema）、recovery 往返 DeepEqual 与篡改拒绝测试、重启后续合并测试通过。
- **Evidence**: `internal/state`、`internal/recovery` 测试，`testdata/released` 规则遵循（发布前无需新增 dump，发布版本才需要）。

### AC-12: 未启用请求行为完全兼容
- **Type**: `rule`
- **Given**: 未启用自动合并的请求。
- **When**: 执行既有的创建、评审请求/跳过/提交、编辑、关闭、重开、mergeability、diff、手工 merge（含检查失败/pending/脏工作树场景）。
- **Then**: 所有响应、错误码、HTTP 状态、Git 效果与本特性前完全一致；检查/评审仍 advisory，不出现于合并阻塞项；JSON 中无 `auto_merge` 字段；旧备份格式仍可读取。
- **Pass Condition**: 既有 PR 相关测试套件全部不改语义通过（必要的仅为新增字段的夹具更新），手工 merge 在检查失败时仍成功。
- **Evidence**: `go test ./internal/pullrequest/... ./internal/server/... ./cmd/owngit/... ./internal/recovery/...` 及代码审查。

### AC-13: 鉴权与请求契约一致
- **Type**: `rule`
- **Given**: 开放模式与共享密码模式两种访问配置。
- **When**: 访问新 API 端点与浏览器表单。
- **Then**: API 端点复用 `authorizeAPI`（开放模式免凭据、密码模式 Basic 校验、速率限制、setup 未完成 409），仅接受 `application/json` 与 `{}` 体，未知字段/非 JSON/超限体按既有规则拒绝；浏览器表单经会话 + CSRF；共享链接只读访问不能启用/取消。
- **Pass Condition**: 鉴权、CSRF、内容类型、share-link 拒绝均有 server 测试。
- **Evidence**: `internal/server` API/会话测试。

### AC-14: 浏览器呈现质量与可访问性
- **Type**: `rubric`
- **Dimension**: 启用/取消控件与等待/阻塞状态在 PR 页面与列表上的清晰度、可访问性与外观一致性。
- **Scale**: 1-5
- **Anchors**: 1 = 无法辨识自动合并状态或控件不可用；3 = 功能可见但状态文案含混、仅靠颜色区分或键盘流程受阻；5 = 状态与每种原因码均有图标+文案双重表达（不依赖颜色）、键盘可完成启用/取消、焦点可见、Light/Dark/System 均清晰、英文与韩文文案齐备且与证据页既有措辞一致。
- **Pass Threshold**: >= 4
- **Evidence**: 浏览器行为测试（`internal/server` web_behavior 风格）+ 三外观/两语言人工或截图核查。

### AC-15: 并发与恢复设计质量
- **Type**: `rubric`
- **Dimension**: 锁次序、幂等、资源边界与代码复用质量。
- **Scale**: 1-5
- **Anchors**: 1 = 引入新锁/时序假设或复制合并 machinery；3 = 功能正确但存在忙等轮询、goroutine 无退出保证或部分另起炉灶；5 = 完全复用仓库写锁与 receipt/merge-intent 幂等链，唤醒合并、无意图零 Git 活动、扫描有界公平、后台循环可取消可等待、失败只留下可重试状态。
- **Pass Threshold**: >= 4
- **Evidence**: 设计走查 + 竞态/关闭序列测试 + 代码审查（与既有 `checkrun.Coordinator`、`completePublishedMergeLocked` 模式对照）。

## Open Questions

- [x] 证据"未过期"的含义：采用现有投影新鲜度（同修订、配置一致、取最新终态 attempt），原始日志保留期不门控（需求方 2026-10-15 确认）。
- [x] 无检查配置且无证据时：持续 waiting_for_checks，不自动合并（需求方确认）。
- [x] 关闭对意图的影响：关闭即撤销，重开不自动恢复（需求方确认）。
- [x] 评审是否门控：不门控，继续 advisory（需求方确认）。
