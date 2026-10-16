# 拉取请求自动合并 - 实施计划

任务按依赖排序；每个任务为可独立自验的纵向切片。AC 映射见 `spec.md`。

## Task 1: 持久化基础——schema step 18 与意图表存储
- **Status**: `completed`
- **Completion Evidence**: schema step 18 已追加（`pull_request_auto_merges`，复合 FK→pull_requests 级联）；新增 `PullRequestAutoMerge` 结构与 `Enable/PullRequestAutoMerge/PullRequestAutoMerges/PullRequestAutoMergesAfter/RepositoriesWithPullRequestAutoMerges/Delete` 方法；意图行在 `SetPullRequestClosed`（关闭）与 `CompletePullRequestMerge`（合并）同一事务内删除；仓库删除语句表已加入。TR-1.1：schema 套件（迁移/指纹 1a6f…/140 对象/拒绝矩阵）全部更新通过；TR-1.2：新增 `pull_request_auto_merge_test.go` 5 个测试（幂等、非 open 拒绝、关闭删除/重开不恢复、合并删除、列表/环形分页、级联）通过；TR-1.3：参数化 SQL，`go vet` 通过，`internal/state` 全量测试 66.8s 通过，gofmt 干净。
- **Priority**: high
- **Depends On**: None
- **Description**:
  - 在 `internal/state/store.go` 的 `schemaSteps` 末尾追加 **step 18**（不改动既有步骤），新建表 `pull_request_auto_merges`：
    - `repository_id TEXT NOT NULL, pull_request_number INTEGER NOT NULL, enabled_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, enabled_by TEXT NOT NULL DEFAULT ''（actor JSON，沿用 created_by 的 CHECK 风格）, PRIMARY KEY(repository_id,pull_request_number), FOREIGN KEY(repository_id,pull_request_number) REFERENCES pull_requests(repository_id,number) ON DELETE CASCADE`。
  - `internal/state/pull_requests.go` 新增 `PullRequestAutoMerge` 结构与 Store 方法：
    - `EnablePullRequestAutoMerge(ctx, repo, number, actor, now)`：仅当 PR 存在且 status='open' 时 upsert（重复启用返回既有行，updated_at 不提前于 enabled_at）；closed/merged 返回 `ErrPullRequestStateChanged`；PR 不存在返回 not found 语义错误。
    - `PullRequestAutoMerge(ctx, repo, number) (record, ok, err)`、`PullRequestAutoMerges(ctx, repositoryID string) ([]record, err)`（repositoryID 为空时列全部仓库，供收敛器扫描；走主键/索引，有界）。
    - `DeletePullRequestAutoMerge(ctx, repo, number)`：删除意图行（不存在不报错）。
    - 在 `SetPullRequestClosed` 的同一事务中：关闭（open→closed）时删除意图行；重开不涉及。
    - 在 `CompletePullRequestMerge` 把 open→merged 的同一事务中删除意图行。
  - 常量：`AutoMergeEnabled = "enabled"`（如状态列需要；当前设计单行=启用，原则上不需要状态列，评审时确认最简形态）。
- **Acceptance Criteria Addressed**: AC-11, AC-12
- **Test Requirements**:
  - `rule` TR-1.1: 从 step 17（及更早可迁移版本）库迁移成功，`pull_request_auto_merges` 目录存在且 FK/级联正确；`currentSchemaVersion()==18`；既有指纹校验通过。证据：`internal/state` 迁移测试输出。
  - `rule` TR-1.2: enable 幂等（同一 PR 仅一行、enabled_at 保持首次值）、非 open 拒绝、close 后行删除、reopen 后无行、merge 完成后行删除、PR 删除级联删除。证据：新增 `pull_request_auto_merge_test.go`（或在 pull_requests_test.go 中）全部通过。
  - `rule` TR-1.3: 全部新 SQL 经注入夹具验证错误路径（DB 错误包装为 state_unavailable 风格），无字符串拼接用户输入。证据：代码审查 + 测试。
- **Notes**: 遵循 `docs/DEVELOPMENT.md`：步骤文本不可变；发布前不新增 testdata/released dump。

## Task 2: 备份恢复可移植性与备份格式 v13
- **Status**: `completed`
- **Completion Evidence**: RecoveryState 新增 `PullRequestAutoMerges` 并接入有序读取/校验/恢复写入与恢复删除顺序表；`ValidatePullRequestRecovery` 增加不变量（意图必须引用存在的 open PR、时间合法、actor 合法、无重复）。备份清单新增 `pull_request_auto_merges` 字段与记录类型，backupVersion 提升至 13，新增 `format13Content` 门控（无意图仍写 v12），旧版本含该记录在 `validateManifest` 被拒，`validateBackupVersion` 接受 13。TR-2.1：新增 `internal/recovery/auto_merge_test.go` 两个端到端测试（v13 往返 DeepEqual+二次备份保持 v13、降级 v12 清单被拒）通过；TR-2.2：`TestPullRequestAutoMergeRecoveryValidation` 四种篡改夹具全部被拒；TR-2.3：既有恢复套件与行迭代故障注入（已加新表）全部通过；state 34.8s / recovery 169.4s 全量绿，gofmt 干净。
- **Priority**: high
- **Depends On**: Task 1
- **Description**:
  - `internal/state/recovery.go`：`RecoveryState` 增加 `PullRequestAutoMerges []PullRequestAutoMerge`；在 `(*PortableRead).finish` 的读取序列中加入新表有序 SELECT；`restorePullRequestRecovery` 增加 INSERT；`RestoreRecoveryState` 的显式删除顺序表在 `pull_request_merge_intents` 之前/同组加入新表（子表先于父表原则）。
  - `ValidatePullRequestRecovery`（pull_requests.go）增加不变量：每条意图引用的 PR 存在且为 open；任一 merged/closed/creating PR 不得携带意图行；actor 合法、时间单调。
  - `internal/recovery/recovery.go`：Manifest 增加 `pull_request_auto_merges,omitempty` 字段；`addPullRequestState`/`recoveryState` 增加映射；`backupVersion` 提升为 13；按 `format12Content`/`backupManifestVersion` 既有模式实现：无意图记录时仍写 v12（新增 format13 content-gate 判定），有记录时写 v13 并在 `validateManifest`/`validateBackupVersion` 拒绝未知新版本；旧版本（v10-v12）清单读取保持兼容（字段缺省=无意图）。
- **Acceptance Criteria Addressed**: AC-11
- **Test Requirements**:
  - `rule` TR-2.1: 含启用意图的备份→恢复→再备份往返 DeepEqual；v12 旧清单（无该字段）可正常恢复；含意图记录强制 v13 写出；v13 清单被旧版本逻辑拒绝。证据：扩展 `internal/recovery/recovery_test.go` 风格的往返/降级测试。
  - `rule` TR-2.2: 篡改清单（意图指向缺失/非 open PR、closed PR 携带意图）被 `ValidatePullRequestRecovery` 拒绝，各一个夹具。证据：state recovery 校验测试。
  - `rule` TR-2.3: 恢复到非空目标被拒、删除顺序不违反 FK 的既有行为不回退。证据：既有恢复测试通过。
- **Notes**: 凭据/会话类仍不导出；意图只含 actor/时间，无敏感字段。

## Task 3: 领域投影与启用/查看/取消服务方法
- **Status**: `completed`
- **Completion Evidence**: types.go 新增 AutoMerge 视图与 waiting/blocked 九类原因码；automerge.go 实现纯投影函数（含 absent/stale/tested_commit/config/read_failure 全形态）、浅/深评估（show 才跑试合并）、Enable/Cancel 服务方法（发布优先对账、幂等、auto_merge_not_enabled、取消时丢弃未发布计划）；setClosed 提交后清理未发布计划；View 新增 omitempty auto_merge。TR-3.1：15 行纯函数表驱动测试通过；TR-3.2：幂等/非 open 拒绝/重复取消/计划清理测试通过；TR-3.3：冲突夹具证明 list 浅评估不试合并（reconciling）而 show 深评估给出 merge_conflict；无意图视图无字段；pullrequest 全量 68.9s 绿；TR-3.4：关闭删除+重开不恢复有测试；receipt 已发布竞态（Enable/Cancel 补登 merged、不留意图行）有测试；gofmt 干净。
- **Priority**: high
- **Depends On**: Task 1
- **Description**:
  - `internal/pullrequest/types.go` 新增：
    - `AutoMergeStatusWaiting="waiting"`、`AutoMergeStatusBlocked="blocked"`；原因码常量：`waiting_for_checks`、`checks_running`、`evidence_unreadable`、`merge_conflict`、`repository_unavailable`、`reconciling`（list 浅评估用）、`checks_failed`、`checks_untrusted_worktree`、`checks_configuration_changed`。
    - `AutoMerge` 视图结构：`enabled`、`status`、`reason`、`message`、`source_oid`、`target_oid`、`enabled_at`、`enabled_by`（omitempty）。
    - `View.AutoMerge *AutoMerge json:"auto_merge,omitempty"`。
  - 新文件 `internal/pullrequest/automerge.go`：
    - `evaluateAutoMergeEvidence(record, source, checks) (status, reason)`：纯函数，把既有 `Checks` 投影映射为门控结论——read_failure→waiting/evidence_unreadable；非 commit 头→blocked 复用 missing/not_commit 码；absent 或 stale(旧修订)→waiting/waiting_for_checks；pending→waiting/checks_running；终态非 passed→blocked/checks_failed；passed 但 !tested_commit（worktree/cleanup）→blocked/checks_untrusted_worktree；passed+tested 但 stale（配置变化）→blocked/checks_configuration_changed；通过→空结论。
    - 深度评估 `autoMergeStatusLocked(...)`：证据门控 + mergeability（show/收敛用，允许 trial merge）；浅评估 `autoMergeStatusShallow(...)`（list 用：不做 trial merge，证据已通过时给 waiting/reconciling）。
  - Service 方法（均走仓库锁，与现有方法同款 problem 包装）：
    - `EnableAutoMerge(ctx, repo, number, actor state.Actor) (*View, error)`：写锁；先 `completePublishedMergeLocked`（若 receipt 已发布则补登 merged 并按 merged 处理，返回该视图/问题，不留新意图）；要求 open；幂等 upsert；返回视图。
    - `CancelAutoMerge(ctx, repo, number) (*View, error)`：写锁；先 `completePublishedMergeLocked`——若已发布则返回 merged 视图并以既有 `pull_request_merged` problem 通告（绝不报取消成功）；无意图行返回新问题码 `auto_merge_not_enabled`（409 类）；有行则删除并安全丢弃该 PR 未发布的 merge-intent 计划（复用 `validateProtectedMergeObjects` + `DeleteUnpublishedPullRequestMerge` 的既有判据，不碰 receipt）；返回无 auto_merge 的视图。
  - 集成点：`setClosed` 在 open→closed 分支内（完成 published 对账之后、`SetPullRequestClosed` 同事务）确保意图删除（Task 1 的 Store 事务已覆盖，服务层补一次未发布计划清理）；`summaryForHeads`（list）与 `viewForHeads`（show）按意图记录装配 `AutoMerge`：无行=nil；show 用深评估、list 用浅评估；merged/closed 视图不携带 auto_merge。
  - 人工 `Merge` 路径不做任何检查门控改动；其 open→merged 后的意图行清理由 Task 1 的事务完成。
- **Acceptance Criteria Addressed**: AC-1, AC-2, AC-3, AC-5, AC-6, AC-12
- **Test Requirements**:
  - `rule` TR-3.1: `evaluateAutoMergeEvidence` 对 spec FR-5/6/7 列举的每一投影形态给出表中原因码（含 passed+dirty、passed+config-stale、pending、absent、stale-old-revision、read_failure、各终态失败、非 commit 头）。证据：纯函数表驱动测试。
  - `rule` TR-3.2: enable 幂等/非 open 拒绝；cancel 成功/重复 cancel 返回 `auto_merge_not_enabled`；cancel 时存在未发布计划被清理、存在 receipt 时补登 merged 且不返回取消成功。证据：`automerge_test.go`（真实 git 夹具，参考 close_test.go 手法）。
  - `rule` TR-3.3: show 返回深评估（含 trial merge 结论），list 返回浅评估且不产生 merge-tree 对象/无新 ref；无意图视图 JSON 不含 auto_merge；既有手工 merge 在 checks failed/pending 下仍成功。证据：服务测试 + 测试中断言 objects 目录/ref 无新增。
  - `rule` TR-3.4: 关闭开放请求时意图行在同一状态翻转后消失，重开不恢复。证据：服务+存储测试。
- **Notes**: message 英文文案在领域层给出稳定句子；浏览器再按原因码本地化（与 blocker 同款做法）。

## Task 4: 锁内自动合并发布路径与收敛方法
- **Status**: `completed`
- **Completion Evidence**: 人工 Merge 与自动合并共享提取出的 `mergeCurrentPairLocked`（行为不变，既有全部测试通过）；自动路径带发布前/计划前双重意图守卫，守卫失败丢弃计划并返回 auto_merge_aborted；`ConvergeAutoMerge`/`ConvergeAutoMerges`（环形分页）实现，等待/阻塞零错误返回、不可用可重试、发布者以启用者 actor 入账。TR-4.1 干净通过即合并、receipt/merged_by 与人工一致、二次收敛幂等；TR-4.2 等待/失败零 Git 写效果；TR-4.3 源推进旧证据不复用→冲突等待→目标解决后按最新 pair 合并且旧 pair 无 receipt；TR-4.4 守卫直接中断测试 + 发布栅栏下排队 cancel 确定性输给已发布合并（无 sleep，channel 同步）；TR-4.5 旧 git runner 不可用等待、恢复后合并；pullrequest 全量 95.5s 绿，gofmt/vet 干净。
- **Priority**: high
- **Depends On**: Task 3
- **Description**:
  - 在 `internal/pullrequest` 中抽出/新增仅供后台使用的合并入口（建议 `autoMergeLocked`，由写锁内调用；从现有 `Merge` L433-538 提取共享的"绑定修订→eligibility→计划→保护对象→发布→对账"内部段，保持人工 Merge 行为逐字节不变）：
    - 入口前读当前头与深评估（证据+合并方法+trial）；waiting/blocked 直接返回原因，不写任何对象。
    - 通过后执行计划/发布；**发布前在同一锁持有期内复查意图行仍存在且 PR 仍 open**（Store 新查询），否则丢弃计划返回 `auto_merge_canceled` 类内部结果，不发布。
    - 发布后走 `reconcileIntentLocked`（CompleteMerge 传启用者 actor）。
  - 新服务方法 `ConvergeAutoMerge(ctx, repo, number) (merged bool, view *View, err error)`：
    - 取写锁（`lockForRequest` 语义：busy/unavailable 映射为 waiting/repository_unavailable 的可重试错误，不作为失败升级）；读意图行，无行则 no-op；读 PR，非 open 则删除残留行并 no-op；读头失败/试合并失败按 FR-6 分类（Git 忙/不可用→可重试错误；冲突→视图 waiting 且 nil error）。
    - 源/目标头与计划期望不一致时永不发布：复用 `publishMerge` 的 head `verify` 事务与 stale 复查作为最后权威。
  - 仓库级批量入口 `ConvergeAutoMerges(ctx, repo, after int64, limit int) (next int64, remaining bool, err error)`：公平游标遍历该仓库意图行（参考 `ObserveCurrentRevisionsAfter` 的环形分页），单个请求 blocked/waiting 不中断整批；仓库级 Git 错误（不可用）提前返回可重试。
  - 目标推进/源推进序列、未发布计划丢弃、receipt 归属全部复用既有函数，不新增平行机制。
- **Acceptance Criteria Addressed**: AC-4, AC-5, AC-6, AC-7, AC-9, AC-10
- **Test Requirements**:
  - `rule` TR-4.1: 合格证据→`ConvergeAutoMerge` 发布合并，MergeResult/receipt 与人工合并格式一致，merged_by=启用者；目标 ref 与 receipt 恰一份。证据：真实 git 集成测试。
  - `rule` TR-4.2: waiting/blocked 全部原因码下 Converge 无 Git 写效果（目标 ref、receipt、merge 对象不新增）。证据：ref/对象断言。
  - `rule` TR-4.3: 源推进后旧证据不被复用（waiting）；目标推进致冲突→waiting/merge_conflict；目标再推进解冲突→同方法合并最新 pair；旧 pair 无 receipt。证据：三阶段序列测试。
  - `rule` TR-4.4: 注入发布栅栏强制两种竞态次序：意图先删→发布被放弃（无 receipt、PR 仍 open/closed）；receipt 先成→取消/关闭补登 merged、返回既有 merged 语义、不二次合并。证据：确定性竞态测试（无 sleep，用回调栅栏）。
  - `rule` TR-4.5: 仓库忙/不可用返回可重试分类且意图保留；冲突不做忙等标记；恢复后同一调用成功。证据：故障注入测试。
- **Notes**: 人工 Merge 与自动路径共享代码段是本任务的核心约束（AC-15 rubric 关注点）。

## Task 5: 后台收敛器、检查完成唤醒与进程装配
- **Status**: `completed`
- **Completion Evidence**: 新增 `internal/automerge` 包：cap-1 合并 channel、5 分钟兜底 ticker、Start 即扫（重启/恢复后续合并）、环形游标公平分页、Stop 等待在跑扫描、Wake 空操作保护。三条终态链路接线：server App 新增 OnCheckCompletion（helper completeAttempt、runner complete/unavailable）、checkrun coordinator 本地 worker 完成回调；main.go 构造 coordinator 并接入 noteChange/OnReady/OnReceive/PR OnChange 与 App/Coordinator 钩子，按 shutdown clock 生命周期管理。TR-5.1/5.3/5.4：5 个 fake 单测+2 个真实 Store/Service/Git 端到端测试（无证据不合并→证据后 Scan 合并→再扫幂等；Start+Wake 异步合并）通过；TR-5.5：checkrun 39s 全量绿、automerge 2.1s 绿；全量 build/vet 通过。TR-5.2 的 HTTP 终态入口在 Task 6 服务器测试中覆盖。
- **Priority**: high
- **Depends On**: Task 4
- **Description**:
  - 新包 `internal/automerge`：`Coordinator`（参照 `checkrun.Coordinator` 的 cap-1 合并 `wake chan struct{}` 模式）：
    - 字段：`Store *state.Store`、`PullRequests`（定义最小接口：`ConvergeAutoMerges` 等，避免反向依赖）、`Logf`、`Interval`（分钟级兜底，默认如 5m）、批大小常量。
    - `Start(ctx)` 恢复后做一次全量扫描并启动 loop；`Stop()` 取消并等待；`Wake(repositoryID)` 非阻塞合并发送（repoID 可用于收窄，实现可先全局扫描有意图仓库，保证无意图仓库零 Git 活动）。
    - loop：wake 或 tick → 列出有意图的仓库（Store 查询）→ 逐仓库游标收敛到完成；单仓库不可用仅记录并继续。
  - 检查终态唤醒接线：
    - `internal/server/App` 增加 `OnCheckCompletion func(repositoryID string)`（nil 安全）；在 helper `completeAttempt`（checks.go L195 成功终态写入之后）与 runner complete/unavailable（configured_checks.go L454/L469 成功之后）调用。
    - `checkrun.Coordinator` 增加同类回调（或复用既有 wake 后由 main 串联）：本地 `executeLocal` 终态完成（coordinator.go L1151 之后）与中断/租约恢复终态点唤醒自动合并；main.go 把两处回调都接到 `autoMerger.Wake`。
  - `cmd/owngit/main.go` 装配：在 pullRequests/checkCoordinator 之后构造 coordinator；`noteWrite`/`noteChange` 中加入 `autoMerger.Wake(id)`（推送、OnReady 恢复、导入发布由此覆盖）；`StartPreparation` 的恢复回调之后触发一次唤醒；生命周期与 checkCoordinator 同款（context 取消 + shutdown.wait）。
- **Acceptance Criteria Addressed**: AC-4, AC-8, AC-10, AC-11, AC-15
- **Test Requirements**:
  - `rule` TR-5.1: 重复 wake 合并为一次扫描；门控满足时一次唤醒内完成合并；紧接 N 次重复通知后目标 ref/receipt/merge-intent 仍恰一份。证据：coordinator 测试（channel 合并计数 + 真实服务夹具）。
  - `rule` TR-5.2: 三个终态入口（helper HTTP、runner HTTP、本地 worker）写入终态证据后自动合并发生，无需任何手工 PR 调用。证据：server/coordinator 集成测试（runner/helper 可用既有 HTTP 夹具；本地途径用协调器测试夹具）。
  - `rule` TR-5.3: 无启用意图时任意唤醒不产生 Git 进程/写效果（spy Store/Manager 断言只发生意图列表查询）。证据：coordinator 测试。
  - `rule` TR-5.4: Start/Stop 干净退出（不泄漏 goroutine）；Start 时已有等待意图则完成一次全量扫描；不可用仓库不阻断其他仓库。证据：生命周期测试。
  - `rubric` TR-5.5: 并发与恢复设计质量；scale 1-5；anchors 1/3/5 同 AC-15；threshold >= 4；证据：对照 `checkrun.Coordinator` 模式的代码审查 + 竞态/关闭测试集。
- **Notes**: 禁止 `time.Sleep` 同步（CI golangci-lint 规则）；测试用可控 channel/时钟。

## Task 6: Owner JSON API 端点
- **Status**: `completed`
- **Completion Evidence**: api.go 路由文法新增 4 段 `auto-merge`（POST）与 5 段 `auto-merge/cancel`（POST），其他方法 405、未知动作 404；handler 复用 `{}` 空体与 generalAccessActor，返回 SuccessEnvelope；`auto_merge_not_enabled`→409。TR-6.1/6.2/6.3：新增 auto_merge_api_test.go（启用/show/list 携带、幂等、取消与重复取消 409、非 open 409、缺失 404、GET 405、非 JSON 415、未知路由 404）+ helper 终态完成触发 OnCheckCompletion 测试，全部通过；server 全量套件 717s 绿（一次 600s 超时为与本特性无关的网络拨号负载 flake，单测 1.9s、长超时复跑通过）。
- **Priority**: high
- **Depends On**: Task 3
- **Description**:
  - `internal/server/api.go`：
    - 路由文法 `parsePullRequestAPIRoute`（L406-439）支持 4 段新增 `auto-merge`（POST=enable）与 5 段 `auto-merge/cancel`（POST=cancel）；其他方法 405；保持 query 白名单机制。
    - `handleAPI` PR 分支增加两个 operation case：解码空 `{}`（与 close/reopen 同款 CSRF 等价体），置 `Actor: generalAccessActor`，分别调用 `EnableAutoMerge`/`CancelAutoMerge`，成功返回 `SuccessEnvelope{PullRequest}`；错误经 `writeAPIError`+`apiStatus`。
    - `apiStatus` 增加新问题码映射：`auto_merge_not_enabled`、enable 时 `pull_request_merged/not_open` 等复用 409 语义；保持既有映射不变。
  - 确认 view JSON 新增 `auto_merge` 对象随 show/list envelope 输出，`DisallowUnknownFields` 不影响响应侧。
- **Acceptance Criteria Addressed**: AC-1, AC-3, AC-13
- **Test Requirements**:
  - `rule` TR-6.1: 两端点 method/path/body/响应 envelope 正确；enable 幂等；cancel 重复返回稳定错误码与 4xx 状态；非 open 请求的 enable 被拒。证据：扩展 server API 测试（含 `TestPRCommandsUseRemoteJSONAPI...` 风格的精确 method/path 断言）。
  - `rule` TR-6.2: 密码模式要求 Basic（401 路径）、开放模式免凭据、setup 未完成 409、非 JSON/未知字段/非空体 4xx、share 只读凭据不可用。证据：鉴权/契约测试。
  - `rule` TR-6.3: 未启用请求的既有 API 响应字节级/字段级不含 auto_merge（夹具比对）。证据：API 兼容性测试。
- **Notes**: 不新增读取端点——状态复用 show/list。

## Task 7: 浏览器拉取请求页面启用/取消与状态呈现
- **Status**: `completed`
- **Completion Evidence**: repositories.go 新增两条 POST 路由（auto-merge / auto-merge/cancel），与既有 PR 动作同走 requireGeneral 门禁与 parseForm+CSRF；handlePullRequestAction 增加 enable/cancel 分支并 303 noticeRedirect；pages_evidence.go 新增 AutoMergePanel 与列表行 AutoMerge 标记；pull-request.html 新增卡片（图标+文案双编码等待/阻塞、原生表单按钮键盘可达），pull-requests.html 列表加自动合并 pill；九类原因码+标题/按钮/帮助/通知的 en/ko 文案齐备；CSS 用 var(--*) 适配 Light/Dark。TR-7.1/7.2：auto_merge_browser_test.go（启用表单、CSRF 403、重定向 notice 状态确认、en/ko 文案、取消后恢复启用表单、后台合并后页面无面板）通过；路由统一门禁保证分享只读不可 POST；webui 套件绿。
- **Priority**: medium
- **Depends On**: Task 3, Task 6
- **Description**:
  - `internal/server/repositories.go` PR 路由分支（L376-418）与 `pull_requests_browser.go`：
    - 新增 POST `.../{n}/auto-merge` 与 `.../{n}/auto-merge/cancel`（parseForm+requireCSRF），进入 `handlePullRequestAction` 风格的处理，调用服务方法后 303 noticeRedirect（notice 键如 `auto_merge_enabled`/`auto_merge_canceled`，经 `pullRequestNotices` 状态确认机制防伪造）；错误走 `browserPullRequestProblem`，新增原因码到本地化消息的映射。
  - `internal/webui/pages_evidence.go`：`PullRequestPage` 增加 AutoMerge 面板字段（启用状态、原因、两动作 URL、启用时间/者）；`PullRequestRow` 增加启用标记与等待/阻塞简示；新增对应的 template 片段（PR 页模板）。
  - `internal/webui/messages_evidence.go`：为每种 waiting/blocked 原因码与动作通知添加 en/ko 文案；列表/详情状态不允许仅用颜色区分（图标+文字）。
  - `renderPullRequest` 装配：open 且有意图时输出面板与取消表单；open 无意图时输出启用表单；merged/closed 不输出动作。
- **Acceptance Criteria Addressed**: AC-1, AC-2, AC-3, AC-14
- **Test Requirements**:
  - `rule` TR-7.1: 页面在无意图/等待/阻塞/merged 四种状态下渲染正确控件与原因文案；启用/取消表单要求 CSRF，缺失返回 403；share 链接只读视图无表单。证据：浏览器处理器测试。
  - `rule` TR-7.2: 动作成功后重定向且 notice 仅在状态确认时显示；已 merged 的竞态显示 merged 而非取消成功。证据：处理器测试。
  - `rubric` TR-7.3: UI 质量；scale 1-5；anchors 1/3/5 同 AC-14；threshold >= 4；证据：英文/韩文案对照、键盘操作流程与 Light/Dark/System 外观人工核查（实际界面，不仅截图）。
- **Notes**: 遵循 AGENTS.md：有意义 UI 变更需在相关尺寸下检查键盘访问、焦点与可见结果。

## Task 8: CLI `owngit pr auto-merge` 子命令
- **Status**: `completed`
- **Completion Evidence**: pr.go 新增 `auto-merge` 子命令（--number 必填、--cancel 开关、无位置操作数、usage 更新）；operations.go 新增两个 POST 端点助手（`{}` 体、原样透传 JSON）。TR-8.1：新增 pr_auto_merge_test.go（真实 in-process 服务器端到端启用/show 携带 waiting_for_checks 与当前 OID/取消、重复取消透传 auto_merge_not_enabled）+ 精确 method/path 断言已加入 TestPRCommandsUseRemoteJSONAPI；TR-8.2：缺 --number 与多余操作数返回 invalid_arguments；cmd/owngit 全量套件（LANG=C）161.8s 绿。
- **Priority**: medium
- **Depends On**: Task 6
- **Description**:
  - `cmd/owngit/pr.go`：在 `prCommand` switch（L38-59）新增 `auto-merge`：共享 `addGeneralRemoteFlags`；必填 `--number`；动作开关 `--cancel`（默认启用）；无位置操作数；加入 usage 文本。
  - `cmd/owngit/operations.go`：新增 `enablePullRequestAutoMerge`/`cancelPullRequestAutoMerge`，POST 至两个端点，体为 `{}`；成功用 `writeResult` 原样透传 `SuccessEnvelope` JSON；错误走既有 `writeStructuredCommandError`（稳定 code，不含 cause）。
  - show/list 的 auto_merge 新字段随透传自然可见，无需专门渲染。
- **Acceptance Criteria Addressed**: AC-1, AC-3
- **Test Requirements**:
  - `rule` TR-8.1: `auto-merge --number N` 与 `--cancel` 发送精确 method/path/`{}` 体并携带 Basic 凭据；输出为服务端 JSON 原样透传（含 auto_merge 状态）；重复 cancel 的错误码透传且退出非零。证据：扩展 `pr_test.go`（httptest 精确断言 + 端到端夹具）。
  - `rule` TR-8.2: 缺 `--number`、多余操作数、`-h/--help` 行为与既有子命令一致。证据：CLI 参数测试。
- **Notes**: 不新增 `--json`——PR 命令本就输出 JSON。

## Task 9: 文档与变更日志
- **Status**: `completed`
- **Completion Evidence**: CODING_TOOLS.md/.ko.md 新增 "Automatic merge/자동 병합" 小节（三端用法、九类等待/阻塞语义、修订推进、取消/关闭次序、重启与备份保留、未启用语义不变）；AUTOMATIC_CHECKS.md/.ko.md 加入"检查仍 advisory、自动合并另行 opt-in"交叉引用并指向锚点；CHANGELOG.md Unreleased/Added 增加条目。TR-9.1：英文与韩文事实逐项对应，无 planned/unsupported 措辞；`go run ./tools/doclinks` 无输出（链接通过）；无凭据/私有信息。TR-9.2：条目符合 Keep a Changelog。备份文档已以 "pull requests" 整体概括可移植内容，意图作为子记录无需另列。
- **Priority**: medium
- **Depends On**: Task 5, Task 6, Task 7, Task 8
- **Description**:
  - `docs/CODING_TOOLS.md` + `.ko.md`：说明自动合并是每请求 opt-in；启用/取消三端用法；等待与阻塞原因码语义；明确未启用时检查仍仅供参考、评审不门控；源/目标推进与重启/备份后的行为；无证据默认等待。
  - `docs/AUTOMATIC_CHECKS.md` + `.ko.md`：简短交叉引用自动合并如何消费检查投影（终态通过+tested commit+配置新鲜+可读取），不改变检查本身。
  - 如备份文档列出可移植记录类别，`docs/BACKUPS.md`+`.ko.md` 增补一句自动合并意图在备份中保留。
  - `CHANGELOG.md`：按 Keep a Changelog 在未发布版本节增加 Added 条目。
  - 运行 `go run ./tools/doclinks`（或等价检查）确认锚点/链接。
- **Acceptance Criteria Addressed**: AC-14（文案一致性）, AC-12
- **Test Requirements**:
  - `rule` TR-9.1: 英文与韩文文档事实一致（功能、原因码、语义限制逐项对应），无"planned/unsupported"表述；doclinks 检查通过；docs 读取测试（含 Windows 文档测试若涉及代码块）通过。证据：命令输出 + 双语文档 diff 走查。
  - `rule` TR-9.2: CHANGELOG 条目存在且格式符合 Keep a Changelog，不泄露凭据/私有信息。证据：文件走查。
- **Notes**: 公共文档用简洁英语；不依赖私有规划文件。
