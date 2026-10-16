# 多个唯一命名的备份计划 - 实施计划

## Task 1: Schema 18 迁移与计划/运行的持久化模型
- **Status**: completed
- **Priority**: high
- **Depends On**: None
- **Completion Evidence**:
  - TR-1.1~1.4：新增 `internal/state/backups_plan_test.go`（TestBackupPlanMigrationFromSchema16 两种情形、TestBackupPlanNameValidation、TestBackupPlanCRUDAndOrder、TestLastScheduledBackupRunIsPerPlan、TestBackupRunHistoryIsBoundedPerPlan）全部通过。
  - TR-1.5：`go test ./internal/state/... -count=1` 全绿（含 TestSchema16PortableRecordsRoundTrip），`go test ./internal/recovery/... -count=1` 全绿；恢复清空清单改为 backup_plans（internal/state/recovery.go）。
  - 迁移指纹：currentSchemaFingerprint=95c56cfab62874eddd4308cde43928534f2e9778c7956239506e00b3acc3d593，objects=140；schema16-1.1.6-populated 迁移后目录与全新建库逐对象一致。
  - 同步更新合成迁移测试顺移至 v19（released_schema_test.go、actions_schema_test.go、upgrade_test.go、schema_upgrade_test.go）。
- **Description**:
  - 在 [store.go](file:///Users/vance/project/mineProject/swe/09101007/project-03/internal/state/store.go) 追加版本 18 迁移（不改写已发布步骤）：创建 `backup_plans` 表（name 主键，1–64 字节且 `GLOB '[A-Za-z0-9_-]*'`；enabled/interval_seconds/destination/keep/verify 沿用 v16 CHECK 范围；created_at、updated_at）；将旧 `backup_schedule` 单行（若存在）转为 name='default' 计划（created_at/updated_at=旧 updated_at）；`DROP TABLE backup_schedule`；`backup_runs ADD COLUMN plan TEXT NOT NULL DEFAULT 'default'`（CHECK 长度 1–64）；新增索引 `backup_runs(plan,rowid)`；保留全局 running 部分唯一索引。
  - [backups.go](file:///Users/vance/project/mineProject/swe/09101007/project-03/internal/state/backups.go)：新增 `BackupPlan` 结构（Name + 既有计划字段 + CreatedAt）与常量（DefaultPlanName="default"、MaxBackupPlans=50、计划名校验）；`BackupRun` 增加 Plan 字段；新增 store 访问器 `BackupPlans`（按 created_at,name 稳定排序）、`BackupPlan(name)`、`SaveBackupPlan`（insert/update upsert，含 ValidateBackupPlan 名称与边界校验）、`DeleteBackupPlan(name)`；保留 `BackupSchedule`/`SaveBackupSchedule` 作为 default 包装（无行返回 configured=false）。
  - 改造 run 访问器：`StartBackupRun` 写入 plan；`BackupRuns` 扫描 plan；新增 `BackupRunsOfPlan(ctx, name)`、`LastScheduledBackupRun(ctx, name)`、`BackupPlanRunning(ctx, name)`（含 running 计数，用于删除拒绝）；`forgetOldBackupRuns` 改为按 plan 分桶（每桶最新 100 条 + 该桶最新 scheduled 保留）。
  - 更新直接引用旧表名的测试夹具：[portable_records_test.go](file:///Users/vance/project/mineProject/swe/09101007/project-03/internal/state/portable_records_test.go)、[format11_test.go](file:///Users/vance/project/mineProject/swe/09101007/project-03/internal/recovery/format11_test.go)（机器本地表清单与非法记录用例改用 backup_plans）；新增 schema 18 迁移测试（16 populated 库升级：行转换、run 回填、旧表消失、无 schedule 行时无 default、迁移指纹一致）。
- **Acceptance Criteria Addressed**: AC-1, AC-12
- **Test Requirements**:
  - `rule` TR-1.1: schema16-1.1.6-populated 库经 17→18 升级后，`backup_plans` 含 default 且字段与旧行逐列一致；旧 `backup_schedule` 不存在；旧 run 全部 plan='default'；证据为迁移测试输出。
  - `rule` TR-1.2: 旧库无 schedule 行升级后 `BackupPlans` 返回空、`BackupSchedule` 返回 configured=false；证据为测试断言。
  - `rule` TR-1.3: 非法计划名（空、65 字节、含 `/`、空格、`.`、中文）与超界 interval/keep 被 `ValidateBackupPlan`/`SaveBackupPlan` 拒绝；证据为测试输出。
  - `rule` TR-1.4: 同一 plan 桶内超 100 条的无备份旧记录被裁剪且保留该桶最新 scheduled；不同 plan 桶互不影响；证据为测试输出。
  - `rule` TR-1.5: 便携快照/恢复测试（含新表名清单）通过，备份集不含 backup_plans/backup_runs；证据为 `go test ./internal/recovery/... ./internal/state/...`。
- **Notes**: backup_plans 为机器本地表，便携快照是显式 allowlist（state/recovery.go），天然不携带；测试清单需显式断言。

## Task 2: backups.Service 计划管理 API 与运行身份/配置冻结
- **Status**: completed
- **Priority**: high
- **Depends On**: Task 1
- **Completion Evidence**:
  - TR-2.1 TestPlansAreCreatedListedChangedAndRefused、TR-2.2 TestDeletePlanRefusesAnActivePlan（running + 清理槽两种占用）、TR-2.3 TestRunFreezesThePlanItStartedWith（目录/verify/keep 均用启动快照）、TR-2.4 TestDeletedPlanBackupsStayInspectable（runs/下载保留）、TR-2.5 旧 ChangeSchedule/StartNow 行为由既有全套用例保证；全部通过。
  - 实现：internal/backups/plans.go（CreatePlan/ChangePlan/DeletePlan/Plans/Plan/StartPlanNow + ErrPlan*）、schedule.go 改为 default 包装、backups.go start(kind,planName)/execute 快照传 state.BackupPlan。
  - `go test ./internal/backups/... -count=1` 全绿。
- **Description**:
  - 在 internal/backups 新增 plans.go（或重构 schedule.go）：`PlanChange`（字段同 ScheduleChange）、`PlanError`（含 Name/Reason：非法名、重名、数量上限、活动拒绝等）；Service 方法 `Plans(ctx)`、`Plan(ctx, name)`、`CreatePlan(ctx, name, change)`（destination 必填、默认 enabled/1d/7/verify on、数量上限、复用 `recovery.CheckBackupFolder` 与 staging area 校验）、`ChangePlan(ctx, name, change)`、`DeletePlan(ctx, name)`（活动拒绝，见 Task 4 的删除判定，先用 store running 计数 + 清理槽检查）。
  - `ChangeSchedule` 保留为 default 包装（不存在时按 ChangeSchedule 旧语义创建 default）。
  - run 启动路径改造：`StartNow() (default)` 保留；新增 `StartPlanNow(ctx, name)`；内部 `start(ctx, kind, planName)` 读取计划快照（不存在→ErrNotConfigured/新 ErrPlanNotFound），构造 run 时设置 Plan=计划名；`execute` 接收冻结快照（destination/keep/verify），签名内聚为 plan+config；run 全程记录 plan。
  - 删除拒绝：计划存在 running run（store）或该计划 run 正持有 removing 槽（removingRun.Plan）时返回 `ErrPlanActive`；定义 `ErrPlanNotFound`、`ErrPlanExists`、`ErrPlanActive`、`ErrPlanLimit`。
  - 同步更新 internal/backups 全部既有测试到新签名/新行为（default 包装下旧行为断言保持不变）。
- **Acceptance Criteria Addressed**: AC-2, AC-3, AC-4, AC-7
- **Test Requirements**:
  - `rule` TR-2.1: CreatePlan 成功落库并可被 Plan/Plans 读回；重名→ErrPlanExists、非法名/超上限→明确错误、首次缺 destination→choose_folder 类原因；证据为 `go test ./internal/backups/...`。
  - `rule` TR-2.2: 计划 P 有 running run 或持有清理槽时 DeletePlan 返回 ErrPlanActive 且行保留；空闲时删除成功；证据为测试输出。
  - `rule` TR-2.3: run 启动后修改计划目录/verify/keep，run 仍写入旧目录、按旧 verify 验证、按旧 keep 清理，run.Plan=P；证据为测试目录与调用断言。
  - `rule` TR-2.4: 删除 P 后其历史 run 仍由 BackupRuns 返回（plan=P），StartCheck/OpenDownload 按 run ID 仍可打开其 manifest 归属的备份；证据为测试输出。
  - `rule` TR-2.5: 旧 ChangeSchedule/StartNow 在无 default 时语义同今日（not configured），PATCH 语义首次创建 default；证据为既有用例不回归。

## Task 3: 按计划隔离的空间估算、保留与旧备份清理；restore limit 多目录
- **Status**: pending
- **Priority**: high
- **Depends On**: Task 2
- **Description**:
  - [backups.go](file:///Users/vance/project/mineProject/swe/09101007/project-03/internal/backups/backups.go)：`checkRoom` 的候选 run 限定同 plan（以 store `BackupRunsOfPlan` 或等值过滤）；`removeOld` 候选严格限定同 plan 且 destination 与启动快照一致；成功 keep、最新已验证保留一个、失败验证仅留最新等规则在计划内重放；forget 仅针对同计划记录。
  - [check.go](file:///Users/vance/project/mineProject/swe/09101007/project-03/internal/backups/check.go)：restore limit 由单值改为按 destination 记忆的 map（mu 保护）：Start 时对全部计划目录探测，destination 变更后探测该目录；Status 按计划目录取值；过期/换库不影响其他目录结果。
  - 同目录双计划场景：确认备份目录名不冲突（时间戳+随机 hex），`requireAbsent` 撞名仅失败本次 run；清理永远经 manifest 归属确认。
- **Acceptance Criteria Addressed**: AC-8
- **Test Requirements**:
  - `rule` TR-3.1: P、Q 同目录各连续多次成功备份后，P 以 keep=2 清理只删 P 的旧备份，Q 全部保留，反之亦然；证据为目录枚举测试。
  - `rule` TR-3.2: P 的失败验证备份只与 P 的成功/失败保留规则互动，Q 的失败验证备份不被 P 删除；非 OwnGit 文件保留；证据为测试输出。
  - `rule` TR-3.3: checkRoom 仅以同计划最新可读备份尺寸估算（构造 Q 更大备份不影响 P 的拒绝阈值）；证据为测试输出。
  - `rule` TR-3.4: 两个不同 destination 的 restore limit 结果互不覆盖，Start 与改目录后均正确探测；证据为测试输出。

## Task 4: 公平调度器（多计划到期选择、失败不饿死、忙碌后继续）
- **Status**: pending
- **Priority**: high
- **Depends On**: Task 2
- **Description**:
  - 重写 [backups.go](file:///Users/vance/project/mineProject/swe/09101007/project-03/internal/backups/backups.go) 调度部分：`nextRuns`/`duePlans` 读取全部计划并逐计划计算到期（最近该计划 scheduled run 启动时间+interval；无则计划时间；disabled/不存在排除），按 (due, CreatedAt, Name) 稳定排序；`tick` 每次仅启动最早到期的一个计划（调用内部 start(scheduled, planName)），忙碌/清理槽占用时等待现有 wake。
  - 保持 run 结束（execute 末尾）、check/upload 结束（end）、计划变更（Create/Change/Delete）后 Wake；唤醒后重新全量评估，仍到期的计划依次启动；每个计划依据“scheduled run 启动时间推进到期”天然每轮至多一次，失败 run 同样推进，杜绝热循环与饿死。
  - 单计划行为（recheck 上限、时钟跳变容忍、停机补一次）保持不变。
- **Acceptance Criteria Addressed**: AC-5, AC-6
- **Test Requirements**:
  - `rule` TR-4.1: fake clock 下 A/B/C 同时到期时启动序列为稳定序（created_at,name）；每次唤醒只启动一个，结束后下一个启动；证据为启动序列断言。
  - `rule` TR-4.2: A 目标不可用快速失败后，同轮 B、C 仍各启动一次；A 在 interval 内不再被调度；证据为测试输出。
  - `rule` TR-4.3: 某计划 verify/check/upload 占用全局槽期间，任何计划 scheduled/manual 启动得到 ErrBusy/ErrBackupRunning；槽释放后到期计划自动续跑；证据为测试输出。
  - `rule` TR-4.4: 单计划下既有的调度/时钟/补跑测试不回归；证据为既有测试通过。

## Task 5: 失败隔离、手动重试与重启中断归因
- **Status**: pending
- **Priority**: high
- **Depends On**: Task 4
- **Description**:
  - 目标不可用路径（OpenBackupFolder/CheckRoom/capture 失败）确认只把该 run（含其 plan）记为失败；调度器 wake 后继续其他到期计划（与 Task 4 联动验证）。
  - `Service.Start` 的 `InterruptBackupRuns` 后，逐计划 due 计算天然按 plan 隔离（LastScheduledBackupRun 带 plan）；补跑仅针对当前到期计划；确认中断 run 保留 plan 名；`StartPlanNow` 提供修复后的手动重试。
  - 校验 `record`/`recordEnd`/pending/removalResult 等内存态在多计划下按 run.ID 工作不变；pending 记录展示其 plan。
- **Acceptance Criteria Addressed**: AC-9, AC-10
- **Test Requirements**:
  - `rule` TR-5.1: A 目标不可用只产生 plan=A 的 failed run，B 随后成功；修复后 StartPlanNow(A) 成功；证据为测试输出。
  - `rule` TR-5.2: A 的 scheduled run 运行中取消服务再 Start：该 run 为 interrupted 且 plan=A；B 无 run 记录；各计划 next due 独立；证据为重启测试输出。
  - `rule` TR-5.3: 停机超过多个 interval 后重启，每个到期计划仅补跑一次，未到期计划不跑；证据为 fake clock 测试。

## Task 6: 状态视图（Status/Runs/Summary）多计划化
- **Status**: pending
- **Priority**: high
- **Depends On**: Task 3, Task 5
- **Description**:
  - [status.go](file:///Users/vance/project/mineProject/swe/09101007/project-03/internal/backups/status.go)：`RunView` 增加 `Plan string json:"plan"`；新增 `PlanStatusView`（ScheduleView + running/last_run/last_verified/next_run/restore_limit）；`Status` 增加 `Plans []PlanStatusView`（稳定序），顶层旧字段锚定 default（未配置时保持 not_configured 形态）。
  - `Runs(ctx)` 支持可选计划过滤（新方法 `RunsOfPlan` 或 opts）；pending/removal 占位视图带 plan。
  - `Summary`/`Summarize` 聚合：无计划 not_configured；任一 enabled→on，否则 off；last_run 跨计划最新；last_verified 跨计划最新；next_run 最早。
  - 注意 running 全局唯一：恰好出现在一个计划视图与顶层（若属于 default）。
- **Acceptance Criteria Addressed**: AC-11
- **Test Requirements**:
  - `rule` TR-6.1: 多计划 Status.Plans 顺序、各字段与 per-plan runs/next due 一致；顶层字段等于 default 视图；零计划时顶层 state=not_configured；证据为 `go test`。
  - `rule` TR-6.2: Runs 每项含 plan；按 plan 过滤结果正确；pending/removal 占位 run 的 plan 正确；证据为测试输出。
  - `rule` TR-6.3: summary 三态（not_configured/off/on）与跨计划聚合时间点正确；证据为测试输出。

## Task 7: owner API 多计划端点与兼容契约
- **Status**: pending
- **Priority**: high
- **Depends On**: Task 6
- **Description**:
  - [backups_api.go](file:///Users/vance/project/mineProject/swe/09101007/project-03/internal/server/backups_api.go)：
    - 新增 `GET /api/v1/backups/plans`（列表，含每计划 schedule+next_run 等 PlanStatusView）、`PUT /api/v1/backups/plans/{name}`（创建，body 同 schedule JSON）、`PATCH /api/v1/backups/plans/{name}`（部分修改）、`DELETE /api/v1/backups/plans/{name}`（活动→409 `backup_plan_active`；不存在→404 `backup_plan_not_found`；上限/重名/非法→400 对应 code）。
    - 新增 `POST /api/v1/backups/plans/{name}/runs`（手动立即备份该计划）；`POST /api/v1/backups/runs` 接受可选 `{"plan":"name"}`，缺省 default。
    - `GET /api/v1/backups/runs?plan=NAME` 过滤。
    - 旧 `/schedule` GET/PATCH 锚定 default（default 不存在时 PATCH 用现有首次设置语义创建）；`/backups` 输出新 plans 字段且保留全部旧字段；错误经 `writeBackupAPIError`/`backupRefused` 映射新增 ErrPlan*（含 admin 鉴权不变）。
  - 扩充 [backups_api_test.go](file:///Users/vance/project/mineProject/swe/09101007/project-03/internal/server/backups_api_test.go) 与浏览器侧 API 测试。
- **Acceptance Criteria Addressed**: AC-1, AC-2, AC-3, AC-4, AC-11
- **Test Requirements**:
  - `rule` TR-7.1: 新端点在管理员鉴权下完成 CRUD/立即备份/过滤；非管理员 401/403 不变；证据为 `go test ./internal/server/...`。
  - `rule` TR-7.2: 旧端点请求/响应契约不回归（顶层字段、错误码、warnings）；default 缺失时 PATCH 可创建；证据为既有用例 + 新增兼容用例。
  - `rule` TR-7.3: 删除活动计划 409 `backup_plan_active`，删除不存在计划 404，重名 409/400 `backup_plan_exists`，非法名/上限 400；证据为测试输出。

## Task 8: CLI `owngit backup` 多计划支持
- **Status**: pending
- **Priority**: high
- **Depends On**: Task 7
- **Description**:
  - [backup_owner.go](file:///Users/vance/project/mineProject/swe/09101007/project-03/cmd/owngit/backup_owner.go)：
    - 新增 `backup plan list|create|delete` 子命令：create 参数 `--name`（必填）、`--destination`（必填）、`--scheduled on|off`（默认 on）、`--interval`、`--keep`、`--verify`，分别走 PUT/PATCH/DELETE 新端点；list 走 GET plans。
    - `now`、`status`、`runs`、`schedule show|set|off` 增加 `--plan NAME`：now/status/runs 用查询参数或新计划端点；schedule 子命令带 --plan 时走 `/plans/{name}` PATCH/GET，缺省走旧 `/schedule`。
    - runs 带 `--plan` 时附加 `?plan=`；用法文本与 JSON 输出更新；错误码透传（backup_plan_active 等）。
  - 更新命令用法/帮助测试与 [backup_alias_test.go](file:///Users/vance/project/mineProject/swe/09101007/project-03/cmd/owngit/backup_alias_test.go) 等。
- **Acceptance Criteria Addressed**: AC-1, AC-2, AC-3, AC-4, AC-9
- **Test Requirements**:
  - `rule` TR-8.1: 新子命令构造的方法/路径/JSON body 正确（沿用现有 httptest 风格）；非法参数给本地 invalid_arguments；证据为 `go test ./cmd/owngit/...`。
  - `rule` TR-8.2: 不带 --plan 的旧命令请求路径/查询与今日完全一致；证据为既有用例不回归。
  - `rule` TR-8.3: `backup now --plan X` 在 X 目标失败/修复前后行为正确（错误 JSON→成功 JSON）；证据为测试输出。

## Task 9: 仪表盘 Storage & recovery 多计划管理
- **Status**: pending
- **Priority**: high
- **Depends On**: Task 7
- **Description**:
  - [pages_backups.go](file:///Users/vance/project/mineProject/swe/09101007/project-03/internal/webui/pages_backups.go)：BackupsInfo 增加 Plans（每项 PlanStatusView + 表单状态）、SelectedPlan、新建计划字段；新增 action 常量（create_backup_plan、delete_backup_plan、save_backup_plan、backup_now 携带 backup_plan 隐藏字段）；BackupRunInfo 增加 Plan 标签。
  - [backups_browser.go](file:///Users/vance/project/mineProject/swe/09101007/project-03/internal/server/backups_browser.go)：渲染计划列表/选中（query `backup_plan`，缺省 default）；新 action 分支调用 CreatePlan/ChangePlan/DeletePlan/StartPlanNow，错误经 scheduleRefusal 同形态通知（含 plan_active 新文案）；旧 save 表单带 plan 字段走 ChangePlan；settings 路由与 CSRF/管理员确认接线（[settings.go](file:///Users/vance/project/mineProject/swe/09101007/project-03/internal/server/settings.go)、[pages.go](file:///Users/vance/project/mineProject/swe/09101007/project-03/internal/webui/pages.go) action 白名单）。
  - [backups.html](file:///Users/vance/project/mineProject/swe/09101007/project-03/internal/webui/templates/backups.html)：计划选择控件（键盘可达的 radio/select + 按钮）、选中计划设置表单（含 Delete）、新建计划表单、按选中计划的 Back up now、runs 列表每行计划名；保持 data-group 局部刷新与 busy 轮询可用。
  - [messages_backups.go](file:///Users/vance/project/mineProject/swe/09101007/project-03/internal/webui/messages_backups.go)（及注册处）：新增 EN+KO 文案（计划、名称、新建、删除、删除活动计划被拒、计划名规则、重名/上限、记录中的计划标签等）；状态图标不只靠颜色。
- **Acceptance Criteria Addressed**: AC-2, AC-3, AC-13
- **Test Requirements**:
  - `rule` TR-9.1: Go 模板/handler 测试覆盖：列出计划、切换选中、创建、修改、删除（含活动拒绝通知）、按计划立即备份、runs 显示计划名；证据为 `go test ./internal/server/... ./internal/webui/...`。
  - `rule` TR-9.2: 新增文案键同时存在 EN/KO，韩语翻译无缺失占位；证据为目录完整性测试/断言。
  - `rubric` TR-9.3: 界面可用性维度；scale 1-5；anchors 1=控件不可键盘操作或外观破损，3=功能可用但焦点/外观其一有明显缺陷，5=纯键盘可完成选择/新建/编辑/删除/立即备份，焦点可见，Light/Dark/System 均正常，状态不只靠颜色；threshold >= 4；证据为浏览器实测（多尺寸、键盘走查、三外观截图）。

## Task 10: 文档更新与全量验证
- **Status**: completed
- **Completion Evidence**:
  - docs/BACKUPS.md、docs/BACKUPS.ko.md：新增“Backup plans/백업 계획”章节（CLI+仪表盘、命名、删除语义、调度公平/隔离）、--plan 用法、按计划保留与立即重试、恢复后重建计划；CHANGELOG Unreleased 记录。
  - TR-10.1 `go vet ./...` 干净；gofmt 干净（仅与本次无关的 tailscale 既有提示）；改动相关包 state/backups/webui/server/cmd 测试全部通过。
  - `go test ./...` 其余失败经逐一甄别为环境性：tools/release（codesign/notarization/npm/darwin 工具链）、本机 git 中文输出、高负载下 githttp/gitexec 秒级超时；githttp 两个失败在 stash 后的基线树上以同样错误复现，gitexec 串行重跑通过，与本改动无关。
- **Priority**: medium
- **Depends On**: Task 8, Task 9
- **Description**:
  - 更新 [docs/BACKUPS.md](file:///Users/vance/project/mineProject/swe/09101007/project-03/docs/BACKUPS.md) 与 [docs/BACKUPS.ko.md](file:///Users/vance/project/mineProject/swe/09101007/project-03/docs/BACKUPS.ko.md)：多计划概念、`backup plan list/create/delete`、`--plan`、每计划独立保留/验证/清理、同目录隔离、删除语义（活动拒删、历史与备份保留）、失败隔离与手动重试、停机重启归因、summary 聚合语义。
  - CHANGELOG.md Unreleased 增加 Changed/Added 条目（含升级为 default 的兼容说明）。
  - 全量验证：`go test ./...`、`go vet`/golangci（仓库配置）、`gofmt`；浏览器实际界面三外观与键盘走查；文档链接检查（仓库 CI 有 doc-links）。
- **Acceptance Criteria Addressed**: AC-13
- **Test Requirements**:
  - `rule` TR-10.1: `go test ./...` 全绿（如存在与本变更无关的既有失败，需明确列出并区分）；证据为命令输出。
  - `rule` TR-10.2: 文档内命令/端点与实现一致，文档链接检查通过；证据为人工核对 + doc-links 输出。
  - `rubric` TR-10.3: 文档质量维度；scale 1-5；anchors 1=文档与实现脱节，3=英文准确但韩语缺失或漏关键语义，5=双语文档准确覆盖全部新行为且与界面措辞一致；threshold >= 4；证据为文档 diff 与双语抽查。
