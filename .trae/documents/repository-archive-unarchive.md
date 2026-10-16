# 仓库归档（Archive / Unarchive）特性实施计划

## Context（背景与目标）

OwnGit 需要支持把长期停更的仓库**归档**供审计：维护者可从仓库设置页、所有者 API、`owngit repo` CLI 三个入口归档/解除归档。核心约束是归档转换必须与仓库既有的**逐仓库写锁**（`gitexec.RepositoryLock`）处于同一顺序边界：

- 写操作（push、PR 合并、浏览器文件恢复、默认分支变更、导入发布）进行中归档 → 明确返回**忙（409/既有 busy 语义）**且不改变状态；
- 归档成功后，上述写操作以及手动/定时导入，必须在改动任何 refs/objects **之前**被稳定的 `repository_archived` 结果拒绝，不允许部分写入、未决发布或误报成功；
- 浏览、clone/fetch、raw 文件、归档下载、PR/check 历史读取保持可用；名称、提交、记录不变；
- 状态持久化于 SQLite，重启生效、随备份恢复；解除归档后无需修复钩子或清理临时状态；旧库升级默认未归档。

已确认的关键机制：写锁带 write gate（[locks.go](file:///Users/vance/project/mineProject/swe/09101007/project-01/internal/gitexec/locks.go) 的 `LockContext`/`TryLockGated` 拿锁后执行 gate，失败即放锁不推进 generation）；gate 当前只装了存储校验（[storage_lock.go](file:///Users/vance/project/mineProject/swe/09101007/project-01/internal/repository/storage_lock.go#L89-L93)）。导入对象在发布锁之外经 `index-pack` 写入目标库（[publish.go](file:///Users/vance/project/mineProject/swe/09101007/project-01/internal/importsync/publish.go#L471-L534)），需要额外的活动计数闭合竞态。

设计决策（实现者默认）：
- 归档拒绝统一 **409**（与 storage_changed/merge 冲突一致），不带 `Retry-After`；忙仍走既有 503/409 busy 路径。
- 删除、重命名、ref 策略设置属于元数据管理，**归档下仍允许**（Delete/Rename 本就走 ungated 路径）。
- PR 创建/评审/关闭同样持 gated 写锁，归档后一并拒绝并隐藏控件（与 GitHub 行为一致）；内部启动恢复（preparation 写 `refs/owngit/`）与维护命令不拦截。
- 备份格式：当前 `backupVersion=12` **未发布**（workflow 字段也以 content-gate 方式折入 v12），沿用先例把 `archived` 折入 v12 并用 `format12Content` 门禁，不升版本；schema 则按硬性规则**追加第 18 步**。

---

## Phase 0 — 状态层（internal/state）

1. [store.go](file:///Users/vance/project/mineProject/swe/09101007/project-01/internal/state/store.go)：在 `schemaSteps` 末尾追加 `{version: 18, statements: [...]}`，内容为
   `ALTER TABLE repository_policies ADD COLUMN archived INTEGER NOT NULL DEFAULT 0 CHECK (archived IN (0,1))`；更新版本历史注释。
2. [repository_records.go](file:///Users/vance/project/mineProject/swe/09101007/project-01/internal/state/repository_records.go)：`RepositoryPolicy` 增加 `Archived bool`；`readRepositoryRecords` 的 SELECT/scan、`restoreRepositoryRecords` 的 INSERT 增加该列；**不**加入 `IsDefault()`。
3. 新建 `internal/state/archive_records.go`：
   - `RepositoryArchived(ctx, id) (bool, error)`
   - `ArchivedRepositoryIDs(ctx) ([]string, error)`（启动加载）
   - `SetRepositoryArchived(ctx, id string, archived bool, at time.Time) error`：仅 upsert `repository_id,archived,updated_at` 三列（`ON CONFLICT … DO UPDATE`，兄弟列 retain/protect/prefixes 不被触碰）。
4. 同步更新受影响的固定夹具测试：[schema_test.go](file:///Users/vance/project/mineProject/swe/09101007/project-01/internal/state/schema_test.go)（L86 指纹注释与 `currentSchemaFingerprint` 重钉为 schema 18 目录，对象数以实测为准；L223/L288/L290 的 “17” 文案）、[portable_records_test.go](file:///Users/vance/project/mineProject/swe/09101007/project-01/internal/state/portable_records_test.go) 的 INSERT 列表。
5. 新增 state 测试：schema16 发布夹具 → 17 → 18 升级链且 archived 默认 0；upsert 保留其它策略列；`RecoverySnapshot` 往返携带 archived。

## Phase 1 — Manager 归档守卫（internal/repository，新文件 archive.go）

1. 新哨兵 `ErrRepositoryArchived`（独立错误，不包装 `ErrRepositoryBusy`），文案如 “the repository is archived and accepts no writes; unarchive it to continue”。
2. `Manager` 增加 `archiveState`（`sync.Mutex` + `archived map[string]struct{}` + `importing map[string]int`），方法：
   - `LoadArchived(ctx)`：从 store 全量载入（main.go 在 `ClaimStorage` 之后、Reconcile/Preparation 之前调用）；
   - `IsArchived(id) bool`（nil-map 安全，纯内存无 DB I/O）；
   - `BeginRepositoryImport(id) error` / `EndRepositoryImport(id)`：archived → 哨兵，否则计数 ±1；
   - `SetArchived(ctx, id, archived)`：
     - 先 `Store.Repository` 校验存在（不存在 → `ErrRepositoryNotFound`）；已处于目标状态 → 幂等成功；
     - 归档：复用 `waitForWriteLock` + `TryLockGated`（同 default-branch 的 [lockWithin](file:///Users/vance/project/mineProject/swe/09101007/project-01/internal/repository/delete.go#L600-L603)）；忙 → `ErrRepositoryInUse`，存储 gate 错误原样透传；拿锁后在 mu 下检查 `importing[id]>0` → 忙；再写 store + 更新 map；`UnlockWithoutRefChanges()`（不跑任何 Git 命令，不推进 generation）；
     - 解除归档：用 [lockCatalogWithin](file:///Users/vance/project/mineProject/swe/09101007/project-01/internal/repository/delete.go#L584-L598)（ungated TryLock + `VerifyStorageHold`，gated 会自己拒绝自己），store 置 0 + `delete(map)`。
3. 修改 `installWriteGate`：装一个闭包，先 `VerifyRepositoryStorage(id)`（存储错误优先），再查内存 archived → `ErrRepositoryArchived`。
4. [delete.go](file:///Users/vance/project/mineProject/swe/09101007/project-01/internal/repository/delete.go) 删除成功路径清除 map 条目；重命名无需处理（ID 稳定）。
5. [main.go](file:///Users/vance/project/mineProject/swe/09101007/project-01/cmd/owngit/main.go#L510-L516)：ClaimStorage 后调用 `LoadArchived`。
6. 新增 `internal/repository/archive_test.go`：持锁时归档忙且状态不变；归档后 gated 取锁被拒；导入计数>0 时归档忙；幂等；解除归档后立即可写、无钩子/临时状态修复；删除清 map；重命名/删除在归档下允许；重启=重新从 store 加载；存储错误优先于 archived。

## Phase 2 — 导入边界（internal/importsync）

1. [errors.go](file:///Users/vance/project/mineProject/swe/09101007/project-01/internal/importsync/errors.go)：新增稳定码 `CodeRepositoryArchived = "repository_archived"`。
2. [run.go](file:///Users/vance/project/mineProject/swe/09101007/project-01/internal/importsync/run.go)：
   - `execute` 在拿到仓库互斥锁后（L73 附近）、admission/写运行记录之前，对 `!newDestination` 检查 `IsArchived` → 直接返回 `CodeRepositoryArchived`（**不留运行记录、不触网**）；
   - `runPipeline` 在 `ensureDestination`（L286）之前 `BeginRepositoryImport`，`defer EndRepositoryImport`，覆盖建库、index-pack 与发布全程。
3. [publish.go](file:///Users/vance/project/mineProject/swe/09101007/project-01/internal/importsync/publish.go#L682)：`LockContext` 错误先判 `ErrRepositoryArchived` → `CodeRepositoryArchived`（兜底），再走 `stoppedProblem`。
4. [scheduler.go](file:///Users/vance/project/mineProject/swe/09101007/project-01/internal/importsync/scheduler.go)：pump 在 `Preparing` 跳过处（L168）并列跳过 archived（在 Claim 之前，schedule 保持 due、不盖章）；`start`（L209）把 `repository_archived` 与 `CodeBusy` 同等对待（不记录失败、不打日志），解除归档后调度自然恢复。
5. 服务端映射：[import_api.go](file:///Users/vance/project/mineProject/swe/09101007/project-01/internal/server/import_api.go#L619-L621) 409 码列表加入新码；浏览器导入处理器加 archived 提示分支。
6. 新增 `internal/importsync/archived_test.go`：手动 refresh 归档库 → 稳定码且无 run 行；归档与 pipeline 竞态（计数窗口内）→ 归档忙、无对象写入/发布；定时跳过不 claim 不留失败记录；发布期 gate 拒绝注入测试。

## Phase 3 — 其它写入口的拒绝映射

- [githttp/handler.go](file:///Users/vance/project/mineProject/swe/09101007/project-01/internal/githttp/handler.go#L372-L386)：receive-pack 拿锁错误处加 `ErrRepositoryArchived` 分支 → 409 + 新增 `archivedMessage` 纯文本（无 Retry-After）；upload-pack（L395）不动。
- [pullrequest/service.go](file:///Users/vance/project/mineProject/swe/09101007/project-01/internal/pullrequest/service.go#L1179-L1190) `lockForRequest` 加显式 archived 分支 → Problem 码 `repository_archived`（覆盖 merge/create/review/close）；[server/api.go](file:///Users/vance/project/mineProject/swe/09101007/project-01/internal/server/api.go#L549-L578) `apiStatus` 加该码 → 409；浏览器 PR 处理器映射到新消息码。
- 恢复：[restore_api.go](file:///Users/vance/project/mineProject/swe/09101007/project-01/internal/server/restore_api.go#L254-L277) `writeRestoreAPIError` 与 `server/restore.go` 浏览器映射加 archived → 409。
- 默认分支：[repository_admin_browser.go](file:///Users/vance/project/mineProject/swe/09101007/project-01/internal/server/repository_admin_browser.go#L127-L130) 与 [repository_admin_api.go](file:///Users/vance/project/mineProject/swe/09101007/project-01/internal/server/repository_admin_api.go#L129-L131) 加 `ErrRepositoryArchived` case（409）。
- check 协调器：在 [coordinator.go](file:///Users/vance/project/mineProject/swe/09101007/project-01/internal/checkrun/coordinator.go#L462) L462、L695 的 `Preparing` 跳过旁并列跳过 archived，避免 gated revision 观察产生噪声日志（check 执行本身不写仓库 refs，不拦）。

## Phase 4 — 所有者 API（internal/server）

- 路由：避开既有 tarball 的 `"archive"` 资源名，在 [api.go](file:///Users/vance/project/mineProject/swe/09101007/project-01/internal/server/api.go#L86-L126) switch 增加 `case "archived"` → 新处理器；
  `POST /api/v1/repositories/{id}/archived`，body `{"archived": true|false}`，经 `authorizeAdminAPI` + `decodeAPIJSON`（仅 JSON、未知字段拒绝）。
- 处理器放 [repository_admin_api.go](file:///Users/vance/project/mineProject/swe/09101007/project-01/internal/server/repository_admin_api.go)：404 不存在；忙/导入进行中 → 409 `repository_busy`；存储变更 → 既有码；成功（含幂等）返回 `repositoryResponse`。
- [repository_api.go](file:///Users/vance/project/mineProject/swe/09101007/project-01/internal/server/repository_api.go#L22-L47) `repositoryAPIItem` 加 `Archived bool json:"archived"`，由 `app.Repositories.IsArchived(id)` 填充（list/show/create/rename 全部响应自动携带）。

## Phase 5 — CLI（cmd/owngit）

- [repo.go](file:///Users/vance/project/mineProject/swe/09101007/project-01/cmd/owngit/repo.go)：新增 `repo archive` 与 `repo unarchive`，沿用 `addRepoAdminFlags` + `admin.client(...)`，POST `…/archived`（body `{"archived":true/false}`），`writeJSON` 输出；更新 switch、顶部错误文案、`printRepoUsage`。
- 更新 help 表驱动测试（help_test.go）；新增 `repo_archive_test.go`（仿 owner_actions_test.go 的 HTTP 测试服务模式）。

## Phase 6 — Web UI（internal/webui + internal/server 模板渲染）

- 消息：[messages_repository_admin.go](file:///Users/vance/project/mineProject/swe/09101007/project-01/internal/webui/messages_repository_admin.go) 增加归档/解除归档标题、说明、成功与忙/被拒消息码（EN+KO 成对，目录完整性测试强制双语）；session 通知映射注册 `repository_archived`/`repository_unarchived` 跳转通知。
- 设置页：[repositories.go](file:///Users/vance/project/mineProject/swe/09101007/project-01/internal/server/repositories.go#L438-L453) 增加 `settings/archive`、`settings/unarchive` 两个 POST 路由（admin screen 已覆盖 "settings"）；handler 仿 `handleSetDefaultBranch`（parseForm → CSRF → `confirmAdmin` 共享确认门 → `SetArchived` → `noticeRedirect`；忙/拒错误重绘页面）；`RepositorySettingsPage` 加 `ArchiveURL/UnarchiveURL/Archived`；[repository-settings.html](file:///Users/vance/project/mineProject/swe/09101007/project-01/internal/webui/templates/pages/repository-settings.html) 增加一个 `setblock`（归档态显示“解除归档”，未归档显示“归档”，用既有 `adminField`）；归档态禁用默认分支表单。
- 徽标：`RepositoryHeader.Archived`（[pages.go](file:///Users/vance/project/mineProject/swe/09101007/project-01/internal/webui/pages.go#L862)，`baseRepositoryPage` 填充）与 `RepositorySummary.Archived`（[pages.go](file:///Users/vance/project/mineProject/swe/09101007/project-01/internal/webui/pages.go#L713)，`repositorySummary` 填充）；在 repository.html 头部与 overview.html pill 区渲染（图标+文案，不仅靠颜色）。
- 隐藏写控件（靠清空既有 URL 字段，模板已按非空渲染）：PR 合并/评审/关闭/新建入口、restore 的 preview/apply、Import 页的 refresh/start；浏览、raw、tarball、clone、PR 历史保持不变。

## Phase 7 — 备份/恢复（internal/recovery）

- [recovery.go](file:///Users/vance/project/mineProject/swe/09101007/project-01/internal/recovery/recovery.go)：
  - `RepositoryPolicyManifest` 加 `Archived bool json:"archived,omitempty"`（[format11_test.go](file:///Users/vance/project/mineProject/swe/09101007/project-01/internal/recovery/format11_test.go#L370-L381) 字段计数 4→5）；
  - capture：`addRepositoryRecords`（L2037）判定改为 `policy.IsDefault() && !policy.Archived` 才跳过，并写入 Archived；`recoveryState`（L2263）回填；
  - `format12Content`（L2132）增加：任一 `item.Policy.Archived` → “an archived repository”，保证仅归档差异的备份写 v12，而 v11 严格解码（`DisallowUnknownFields`，L1605）会按版本先行拒绝。
- 测试：备份→恢复保留归档；v11/v10 旧备份恢复后默认 false；仅归档的备份选 v12；新版本号拒绝文案不变。

## Phase 8 — 文档与 CHANGELOG

- [CHANGELOG.md](file:///Users/vance/project/mineProject/swe/09101007/project-01/CHANGELOG.md) `## [Unreleased]` 下 Added 条目（英文）。
- [docs/REPOSITORIES.md](file:///Users/vance/project/mineProject/swe/09101007/project-01/docs/REPOSITORIES.md) + REPOSITORIES.ko.md：归档语义、三个入口、409/忙行为、仍可读/clone、CLI 用法；注意供 doclinks 校验的锚点。
- BACKUPS 文档无需改动（格式版本不变）。

## 验证（端到端）

1. `gofmt`/`go vet ./...`；`go build ./...`。
2. 包级测试：`go test ./internal/state ./internal/repository ./internal/importsync ./internal/githttp ./internal/pullrequest ./internal/recovery ./internal/server ./internal/webui ./internal/checkrun ./cmd/owngit`；重点用 `-race` 跑新增并发边界测试（可控持锁、导入计数竞态、归档/解除归档来回切换）。
3. 全量 `go test ./... -short` 后再完整跑一次；Linux race 由 CI 覆盖。
4. 手工端到端（测试夹具仓库）：建库+推送 → 设置页归档 → `git push` 被 409 拒绝且 fetch/clone 成功 → PR 合并/恢复/默认分支/手动导入均 409 → 定时导入不产生运行记录 → 重启服务状态仍在 → `owngit backup` + `owngit restore` 到新状态目录后仍归档 → CLI `owngit repo unarchive` 后推送/调度立即恢复，无需任何修复。
5. `go run ./tools/doclinks`；若新增依赖（无）才跑 notices 校验。

## 风险与边界

- 初始导入竞态由“建库前注册计数 + 发布 gate 兜底”闭合；归档在导入网络抓取/索引窗口内得到忙。
- 备份持读锁期间归档会在 ~2s `waitForWriteLock` 预算后返回忙（状态不变，可重试）。
- 归档/解除归档不运行 Git 命令，必须用 `UnlockWithoutRefChanges`，否则缓存快照被无谓作废。
- 内存 map 只是 DB 的缓存；所有翻转都在写锁下完成，重启以 `LoadArchived` 为权威。
- 新 API 路径经 `repositoryAddressOf` 解析，重命名/别名自动可用。
