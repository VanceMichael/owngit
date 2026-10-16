# 多备份计划 - 独立审查

- **范围**：schema 18 迁移；internal/state、internal/backups、internal/server、internal/webui、cmd/owngit、docs；对照 .trae/specs/multi-backup-plans/spec.md 的 13 条 AC。
- **审查人**：实现后的独立复查（代码走查 + 确定性测试证据 + 全量 go test）。
- **方法**：逐条 AC 核对代码路径与测试；重点复查并发/删除竞态、同目录清理隔离、兼容契约、机器本地数据。

## AC 核对

- **AC-1 升级为 default + 旧入口兼容**：通过。v18 `backup_plans`/`plan` 列；迁移测试覆盖“有/无 schedule 行”两情形（逐列、指纹、旧表消失、run 回填 plan、无 default 时不建）；旧 `/schedule`、`POST /runs`、CLI 无 --plan 均锚定 default（TestBackupAPIStatesAndSchedule、TestBackupCommandsTalkToTheServer、help 测试）。
- **AC-2 三处对等 CRUD**：通过。服务 CreatePlan/ChangePlan/DeletePlan/Plans/Plan；API PUT/PATCH/DELETE/GET /plans[/{name}]（TestBackupPlansAPI）；CLI plan list/create/delete + schedule --plan（TestBackupPlanCommands）；仪表盘选择/新建/保存/删除（TestBackupPlansInTheDashboard）。重名/非法名/上限/缺目录均拒绝。两计划同目录在服务与 API 层均有覆盖。
- **AC-3 删除活动计划拒绝**：通过。running 计数（DB 部分唯一索引）与清理槽两条件；复查发现两处并发问题并修复：①“计数-删除”窗口与 start 的插入未完全串行——DeletePlan 在 s.mu 内完成 running 计数与删除，start 在同一把锁内插入 run；②修复①时一度在持锁状态调用 Wake（同锁）造成自死锁，已改为解锁后 Wake（该死锁由 TestDeletePlanRefusesAnActivePlan 在 15 分钟超时转储中捕获 goroutine 确认）。TestDeletePlanRefusesAnActivePlan 覆盖两种占用。
- **AC-4 身份/冻结/删除后可查验**：通过。run.plan 非空、视图与 API/CLI/仪表盘均显示；execute 仅接收快照值（TestRunFreezesThePlanItStartedWith 断言改目录/verify/keep 不影响在跑 run）；删除计划后 RunsOfPlan 仍返回、OpenDownload 按 ID 可用（TestDeletedPlanBackupsStayInspectable、API/CLI/浏览器三层等价用例）。
- **AC-5 全局单槽跨计划**：通过。task/removing 内存槽 + backup_runs 全局部分唯一索引（schema18 保留）；TestOtherPlansWaitForTheOneSlot 与既有 busy 用例；check/upload 槽与计划无关。
- **AC-6 公平调度**：通过。dueFrom 按 (due, created_at, name) 稳定选择、每唤醒至多启动一个、run 结束 Wake 续跑；TestSchedulerStartsDuePlansInOrderAndContinuesAfterFailure 验证四计划顺序、失败计划 interval 内不热循环。
- **AC-7 运行身份与配置冻结**：通过（同 AC-4；run.plan 在 start 写入并贯穿记录/清理/状态）。
- **AC-8 同目录按计划隔离**：通过。removeOld/checkRoom 只用 BackupRunsOfPlan(run.Plan) 且保留同 destination 过滤；TestPlansSharingAFolderKeepRetentionSeparate 逐备份目录断言 keep=2/3 互不去除、forget 仅本计划；TestRoomEstimateUsesOnlyTheSamePlan 用他计划损坏 manifest 验证空间估算隔离。非 OwnGit 文件保持不动由既有用例覆盖。
- **AC-9 单目标失败隔离与手动重试**：通过。文件占位目标使 aaa-broken 仅自身 failed，bbb/ccc/ddd 随后成功，移除占位后 StartPlanNow 成功（TestSchedulerStartsDuePlansInOrderAndContinuesAfterFailure）。
- **AC-10 重启中断归因与补跑**：通过。InterruptBackupRuns 保留 run.plan；到期只看本计划 LastScheduledBackupRun；TestRestartAttributesTheInterruptedRunToItsPlan 断言中断归 alpha、bravo 无记录、只补跑一次、可手动重试。既有 TestRestartRecordsARunningBackupAsInterrupted 覆盖 default 单计划语义。
- **AC-11 状态视图与 summary 聚合**：通过。Status.Plans 每计划一视图、顶层字段锚定 default、Runs/RunsOfPlan、summary 三态与跨计划聚合（TestStatusAndSummaryCoverEveryPlan）；MCP summary 不泄露目录/错误（TestBackupSummaryNamesNoFolderOrMessage、CLI MCP 精确串用例）。
- **AC-12 机器本地数据**：通过。便携快照 allowlist 天然不含新表；恢复清空清单改为 backup_plans（state/recovery.go）；format11/state portability 测试已改并通过。
- **AC-13 双语文档与界面**：通过。新增 16 条消息均 en/ko；韩文页断言（TestBackupPlansInTheDashboard）；docs/BACKUPS.md 与 BACKUPS.ko.md 同步新增“Backup plans/백업 계획”章节、--plan 用法、按计划保留与删除语义、恢复后重建说明；CHANGELOG 记录。

## 其他复查点

- 性能：Status 一次 BackupRuns 全表读 + 每计划一次 LastScheduledBackupRun（≤50，命中索引）；仪表盘忙时轮询可接受。
- restore limit：改为 map[destination]，Start 时逐计划探测；exFAT 行为沿用宿主能力，未新增自动化用例（环境依赖，与旧实现一致）。
- 计划名字符集为 ASCII 安全集，进 URL 路径/CLI/磁盘均无需额外转义；API 仍做 PathEscape/QueryEscape。
- 仪表盘 GET 选择走 query；POST 结果回跳带 backup_plan；删除表单独立，避免与 hidden action 重名。
- tick 在计划恰于读取后被删时忽略 ErrPlanNotFound，避免噪声日志。

## 全量验证

- `go vet ./...` 通过；改动文件 gofmt 干净（internal/tailscale/tailscale_test.go 的既有 gofmt 提示与本次无关、未改动）。
- 相关包：internal/state、internal/backups、internal/webui、internal/server、cmd/owngit 全部通过（含新增用例）。
- `go test ./...` 仅见环境性失败：tools/release（macOS codesign/notarization/npm/darwin 工具链）、本机 git 中文输出（TestCLIGitFailures… 期望英文 fatal）、高负载下 gitexec/githttp/cmd 的秒级超时用例；相关用例单独/串行重跑通过，与本改动无关。

## 结论

13 条 AC 均有代码与测试证据；复查发现的删除竞态已修复并回归。**通过**。
