# 多个唯一命名的备份计划 - 产品需求文档

## Overview

- **Summary**: 将 OwnGit 目前唯一的一份备份计划（schedule）扩展为多个唯一命名的备份计划（plan），覆盖 Storage & recovery 仪表盘、owner API 与 `owngit backup` 命令行；保持全局单槽串行执行，但调度按到期时间公平选择计划；运行、保留、清理、估算、失败与重启归因全部按计划隔离。
- **Purpose**: 管理员需要同时备份到本机磁盘和离线挂载盘等多个目标，各自有独立的启停、目录、间隔、保留数与验证设置；切换目标不应再覆盖唯一一份计划，且一个计划的失败、忙碌或删除不得影响其他计划。
- **Target Users**: 运行 OwnGit 服务器的管理员（通过仪表盘或 owner API/CLI 操作）；通过 MCP summary 了解备份是否工作的编码工具（间接）。

## Goals

- 支持多个唯一命名计划，每个计划独立保存：启停状态、目标目录、间隔、保留数、是否验证。
- 旧单计划无损升级为 `default`；旧接口与不带计划参数的调用全部继续指向 `default`。
- 全局仍至多一个备份、验证或上传占用执行槽；调度器按到期时间与稳定次序在多个到期计划间公平选择，忙碌结束后继续处理其余到期计划，失败或频繁到期的计划不能饿死其他计划。
- 每次 run 记录计划身份并冻结启动时配置；运行中改计划只影响下一次；删除活动计划被拒绝。
- 计划删除后其历史 run 与已写备份仍可查验；空间估算、验证失败保留、成功保留数与旧备份清理严格按计划隔离，同目录也不互删。
- 单目标不可用只令该计划失败，其他到期计划照常运行；服务中途停止后，重启把中断 run 归回原计划，其他计划不算已执行，目标修复后可对该计划手动重试。

## Non-Goals

- 不允许并行执行多个备份：全局槽保持唯一（备份、验证、上传互斥）。
- 不做计划改名（rename）：名称是稳定身份；需要改名时删除后新建（活动计划删除被拒，天然安全）。
- 不做计划级别的上传槽管理：上传的备份仍全局唯一，存于状态目录的 `backup-uploads/`，与计划无关。
- 不做备份文件在计划间的迁移、合并或认领。
- 不改变离线备份内容边界：备份计划与运行历史仍是机器本地数据，不进入备份集、不随恢复带回。
- 不做备份文件的自动跨计划去重。

## Background & Context

- 现状代码：
  - 持久化：[backups.go](file:///Users/vance/project/mineProject/swe/09101007/project-03/internal/state/backups.go) 中 `backup_schedule` 为 `singleton=1` 单行表；`backup_runs` 有全局部分唯一索引 `backup_runs_running ON (status) WHERE status='running'`，保证跨重启至多一个运行中的 run。
  - 服务：[backups.go](file:///Users/vance/project/mineProject/swe/09101007/project-03/internal/backups/backups.go)（Service/调度/执行/清理/空间估算）、[schedule.go](file:///Users/vance/project/mineProject/swe/09101007/project-03/internal/backups/schedule.go)（ChangeSchedule）、[status.go](file:///Users/vance/project/mineProject/swe/09101007/project-03/internal/backups/status.go)（Status/Runs/Summary）、[check.go](file:///Users/vance/project/mineProject/swe/09101007/project-03/internal/backups/check.go)（再次验证/下载/恢复限制）、[upload.go](file:///Users/vance/project/mineProject/swe/09101007/project-03/internal/backups/upload.go)（上传）。
  - owner API：[backups_api.go](file:///Users/vance/project/mineProject/swe/09101007/project-03/internal/server/backups_api.go)，路由 `/api/v1/backups`、`/runs`、`/schedule`、`/upload`、`/runs/{id}/verify|archive`、`/summary`。
  - 仪表盘：[backups_browser.go](file:///Users/vance/project/mineProject/swe/09101007/project-03/internal/server/backups_browser.go) 与 [backups.html](file:///Users/vance/project/mineProject/swe/09101007/project-03/internal/webui/templates/backups.html)，视图结构在 [pages_backups.go](file:///Users/vance/project/mineProject/swe/09101007/project-03/internal/webui/pages_backups.go)。
  - CLI：[backup_owner.go](file:///Users/vance/project/mineProject/swe/09101007/project-03/cmd/owngit/backup_owner.go)。
  - Schema：最新为未发布的 17；`backup_schedule`/`backup_runs` 建于版本 16（[store.go](file:///Users/vance/project/mineProject/swe/09101007/project-03/internal/state/store.go#L1518-L1548)）。按仓库约定“新版本追加一个迁移步骤”，本次追加版本 18。
  - 备份文件夹名 `owngit-backup-YYYYMMDD-HHMMSS-<8 随机 hex>`，归属以 run 记录的 manifest SHA-256 判定（`openOwned`），同目录天然可区分；清理当前仅按 destination 过滤，需要增加计划维度。
- 现有已具备的相关语义（保持）：scheduled run 的到期时间 = 最近一次 scheduled run 启动时间 + interval（任何结局都“用掉”这次时间，故失败不会热重试）；无 scheduled run 时以 `UpdatedAt` 为到期点；停机期间错过只补一次；run 结束后 Wake 调度器；配置在 `start` 时快照传入 `execute`；重启时 `InterruptBackupRuns` 把 running run 标记为 interrupted。
- 文档现状：[BACKUPS.md](file:///Users/vance/project/mineProject/swe/09101007/project-03/docs/BACKUPS.md) 与韩语版描述单计划；DEVELOPMENT.md 产品原则要求每个管理员任务在仪表盘与命令行同时可用、英文默认并支持韩语、键盘与辅助技术可达、状态不只靠颜色。

## Functional Requirements

- **FR-1 计划模型与持久化**
  - 新增 `backup_plans`：`name`（主键、稳定身份）、`enabled`、`interval_seconds`、`destination`、`keep`、`verify`、`created_at`、`updated_at`，沿用现有 interval/keep/destination 的约束范围。
  - `backup_runs` 增加 `plan` 列（计划名，非空）；全局 running 部分唯一索引保留，新增 `(plan, rowid)` 索引。
  - 计划名规则：1–64 字节，字符集 `[A-Za-z0-9_-]`；全库计划数上限 50（服务层强制）。
  - 历史 run 记录上限按计划各自维护（每计划沿用 100 条 + 每计划保留最新一条 scheduled run 的规则）。

- **FR-2 升级与默认计划**
  - 版本 18 迁移：旧 `backup_schedule` 有行则转为名为 `default` 的计划（created_at/updated_at 取旧 updated_at）；旧 `backup_runs` 全部归 `default`；随后删除旧表。旧表无行则不创建 default（保持“尚未配置”状态）。
  - 旧 store 访问器 `BackupSchedule`/`SaveBackupSchedule` 保留为 `default` 计划的包装，保证既有调用方语义。
  - 旧 owner API（`/schedule` GET/PATCH、`/backups` 顶层字段、`/runs`、无 body 的 `POST /runs`）与旧 CLI（`backup now/status/runs/schedule …` 不带 `--plan`）继续操作 `default`；default 不存在时返回与今日一致的“未配置/choose folder”结果。PATCH 旧 schedule 在 default 不存在时创建它。

- **FR-3 计划管理（API/CLI/仪表盘三处对等）**
  - 管理员可：列出计划（含每计划 state、destination、interval、keep、verify、updated_at、next_run）、创建计划（首次必须给绝对路径 destination，校验与现 ChangeSchedule 相同：绝对路径、不与存储重叠、可创建私有权限；其余默认 enabled=true、1d、keep 7、verify on）、修改计划任一部分、删除空闲计划。
  - 重复名、非法名、超数量上限、缺 destination、相对路径、不可用目录等被拒绝并给出与今日同形态的原因（API code + 仪表盘自措辞通知 + CLI JSON 错误）。
  - 删除活动计划被拒绝：该计划存在 status='running' 的 run，或其刚结束的 run 正占用旧备份清理槽。拒绝为明确类型（API 409 `backup_plan_active`；CLI `backup_plan_active`；仪表盘通知）。
  - CLI：`backup plan list`、`backup plan create --name … --destination … [--scheduled on|off --interval … --keep … --verify …]`、`backup plan delete --name …`；`schedule show|set|off`、`now`、`status`、`runs` 增加可选 `--plan NAME`，缺省为 default。
  - 仪表盘：计划选择（稳定次序）、选中计划的设置表单与 Save、删除按钮、新建计划表单、对选中计划的 Back up now；记录列表每行显示计划名。

- **FR-4 运行身份与配置冻结**
  - 每个 manual/scheduled run 持久化其计划名；RunView/API/CLI/仪表盘记录均可显示计划名。
  - run 启动时快照计划配置（destination、interval 无关执行但 keep、verify、enabled 相关）：运行中修改计划不影响正在运行的 run（写入快照目录、按快照 verify 决定是否验证、按快照 keep 清理），仅影响下一次 run。
  - 计划删除不级联删除 run（plan 为普通字符串列，无外键）；删除后其 run 仍可在 `/runs` 与仪表盘查验，并可按 run ID 发起再次验证（check）与下载（archive）；磁盘上的备份目录不被删除。

- **FR-5 公平调度与全局单槽**
  - 全局槽语义不变：同一时刻至多一个备份、验证或上传；running 互斥由数据库部分唯一索引跨重启保证，验证/上传/清理由服务内槽保证。
  - 每个计划独立计算到期时间：最近一条该计划的 scheduled run 启动时间 + interval；无则取该计划 created/updated 时间；disabled 或不存在的计划永不到期。
  - 选择规则：在所有已到期计划中，选到期时间最早者；到期时间相同按稳定次序（created_at, name）。每次唤醒至多启动一个 run；run（无论成败）或验证/上传/清理结束后继续评估，仍到期的其他计划依次启动。
  - 已结束（含失败）的 scheduled run 用掉其时间片：失败计划在其下一个 interval 前不会再次到期，因此快速失败不会热循环，也不会抢占其他到期计划；手动 run 不推进 scheduled 到期时间。

- **FR-6 按计划隔离的保留、清理与空间估算**
  - `checkRoom` 只以同一计划的历史 run 作为估算参照。
  - `removeOld` 的候选仅限同一计划的 run（同计划 + 快照 destination）：成功保留数 keep、“最新已验证保留一个”、失败验证仅保留最新一个等规则全部在计划内部成立；另一计划即使使用同一 destination，其备份永不成为删除/forget 目标。
  - 删除仍只通过“run 记录 + manifest 归属”确认 OwnGit 写入的目录，非 OwnGit 文件不动。

- **FR-7 失败隔离、手动重试与重启归因**
  - 某计划目标目录不可用（未挂载、打不开、无空间等）时，只记录该计划该次 run 失败；调度器随后继续启动其他已到期计划。
  - 目标修复后，管理员可对该计划手动 `backup now --plan NAME`（API/仪表盘同义）立即重试。
  - 服务在 run 运行中停止：重启时该 run 标记 interrupted，plan 名保留；其他计划不产生任何 run 记录、不被算作已执行；每个计划的下次到期只依据本计划 scheduled runs。重启后仅对“当前确已到期”的计划补一次（与今日单计划停机补跑语义一致，逐计划适用）。

- **FR-8 状态视图与 summary**
  - `/api/v1/backups` 在保留全部既有顶层字段（语义锚定 default）之外，新增 `plans` 数组：每计划一个视图（schedule、running、last_run、last_verified、next_run、restore_limit），按稳定次序排列。
  - `/api/v1/backups/runs` 每项新增 `plan` 字段，支持 `?plan=NAME` 过滤。
  - 手动启动支持 `POST /api/v1/backups/plans/{name}/runs`；`POST /api/v1/backups/runs` 接受可选 JSON `{"plan":"name"}`，缺省 default。
  - `/api/v1/backups/summary`（供编码工具，不含目录/错误细节）聚合定义：无计划为 `not_configured`；任一计划 enabled 为 `on`，否则 `off`；last_run 取全部计划最新一条；last_verified_at 取全部计划最新一条；next_run 取最早一个。
  - restore limit 探测按 destination 记忆（服务启动时探测所有计划目录，改目录后探测该目录），状态视图按计划给出。

- **FR-9 仪表盘可达与双语言**
  - 新增文案均提供英文与韩语目录条目；Light/Dark/System 外观下可用；计划选择、创建、删除、Back up now 可键盘操作；状态指示不只依赖颜色。

## Non-Functional Requirements

- **NFR-1 兼容性**：旧 API 响应不删除既有字段；旧 CLI 用法（无 `--plan`）行为不变；schema 14/15/16 已发布版本可一路升级到 18，迁移有事务与指纹校验保护。
- **NFR-2 可测性**：调度公平性、同目录隔离、重启归因、删除拒绝等均可用 fake clock 与临时目录做确定性 Go 测试；不依赖真实挂载盘。
- **NFR-3 安全边界不变**：所有计划管理接口仅管理员（API 管理员口令、仪表盘管理员确认+CSRF、CLI password-file）；destination 校验、私有权限创建、不与状态/仓库目录重叠等规则对每个计划同等生效。
- **NFR-4 有界性**：计划数 ≤ 50；每计划历史 run 上限 100；清理、估算、列表查询使用索引，不随备份目录内文件数量失控。
- **NFR-5 文档同步**：更新 docs/BACKUPS.md、docs/BACKUPS.ko.md 与 CHANGELOG（Unreleased），说明多计划、`--plan`、plan 子命令、隔离与删除语义。

## Constraints

- **Technical**：Go + SQLite（现代驱动）；schema 迁移只可追加版本 18，不可改写已发布步骤；指纹校验要求迁移后目录与全新建库一致；Windows/macOS/Linux 三平台行为一致（路径、权限）；备份名格式与 manifest 归属机制不变（离线 verify/restore 兼容旧备份）。
- **Business**：英文默认 + 韩语支持；Git 术语与标识符不翻译；不引入新依赖、新服务或外部写操作。
- **Dependencies**：复用现有 recovery、state、webui、apiclient 设施；MCP `backup_status` 走既有 summary 通道。

## Assumptions

- 计划名采用 ASCII 标识符（`[A-Za-z0-9_-]{1,64}`），作为代码型标识符不翻译、不支持空格或韩语；韩语管理员通过仪表盘管理，仪表盘可为计划提供名称之外的展示（直接显示名字）。
- `default` 不享有删除豁免：它可与其他计划一样在空闲时删除；删除后旧 schedule 接口回到“未配置”，旧 PATCH 会重新创建 default。
- 计划数量上限取 50（足以覆盖本机盘+多个挂载盘场景，同时保持状态与 UI 有界）。
- 上传备份（restore 用）继续保持全局唯一，不归属任何计划；对单条 run 的再次验证（check）与下载按 run ID 工作，不要求计划存在。
- 不提供 rename；稳定次序取 `(created_at, name)`。
- 备份目录名沿用现有格式（时间戳+随机 8 hex + manifest 归属），同目录两计划撞名概率可忽略；撞名时 `requireAbsent` 令该次 run 失败并归该计划，不涉及对方备份。

## Acceptance Criteria

### AC-1: 旧单计划升级为 default 且旧入口全部沿用
- **Type**: `rule`
- **Given**: 一个由已发布版本（schema 16）写出的、含 `backup_schedule` 行与历史 `backup_runs` 的状态库
- **When**: 用本版本启动并完成迁移
- **Then**: 存在名为 default 的计划，enabled/interval/destination/keep/verify/updated_at 与旧行一致；所有历史 run 的 plan=default；旧表消失；旧 `GET/PATCH /api/v1/backups/schedule`、`GET /api/v1/backups` 顶层字段、`GET /api/v1/backups/runs`、无 plan 的 `POST /runs`、CLI `owngit backup now/status/runs/schedule show|set|off`（不带 --plan）全部作用于 default；旧库无 schedule 行时 default 不存在且旧接口表现为 not_configured/choose folder
- **Pass Condition**: 迁移测试与 API/CLI 兼容测试通过；`PRAGMA` 确认表/索引结构；全新建库指纹与迁移后一致
- **Evidence**: `go test ./internal/state/... ./internal/server/... ./cmd/owngit/...`（迁移、兼容用例）输出

### AC-2: 三处界面可创建/列出/修改/删除多个唯一命名计划
- **Type**: `rule`
- **Given**: 管理员身份
- **When**: 通过 owner API、`owngit backup plan …`/`--plan` CLI、Storage & recovery 仪表盘分别创建两个以上计划（含同一 destination 的两个计划），再列出、修改各自字段、删除一个空闲计划
- **Then**: 每计划独立持久化 enabled/destination/interval/keep/verify；重复名、非法名（字符集/长度）、超 50 个、首次缺 destination、相对路径、与存储重叠目录均被拒绝并返回明确原因；删除空闲计划后列表不再含它
- **Pass Condition**: 服务层、API（HTTP 状态码与 code）、CLI（JSON/退出码）、浏览器仪表盘（表单与通知）均有用例通过
- **Evidence**: `go test ./internal/backups/... ./internal/server/... ./cmd/owngit/...`；仪表盘浏览器操作截图/DOM 断言

### AC-3: 删除活动计划被拒绝且计划保持不变
- **Type**: `rule`
- **Given**: 某计划有一个 running run（或其结束 run 正持有清理槽）
- **When**: 经 API/CLI/仪表盘删除该计划
- **Then**: 删除被拒绝（API 409 `backup_plan_active`；CLI 同名错误码；仪表盘错误通知），计划行、配置与 run 均保留；run 结束（清理完成）后同一删除成功
- **Pass Condition**: 用例覆盖 running 与清理槽两种占用
- **Evidence**: `go test` 相关用例输出

### AC-4: 计划删除后历史与已写备份仍可查验
- **Type**: `rule`
- **Given**: 计划 P 有若干成功/失败 run 与磁盘备份，随后 P 被删除
- **When**: 调用 `/api/v1/backups/runs`（及 `?plan=P`）、对其 run 发起 verify 与 archive 下载，并在文件系统检查备份目录
- **Then**: run 记录仍在且显示 plan=P；verify/download 按 run ID 正常工作；磁盘备份目录一个不少；离线 `owngit backup verify <目录>` 仍可查验
- **Pass Condition**: 用例验证记录可读、再验证可启动、归档字节正确、备份目录仍存在
- **Evidence**: `go test` 用例与文件系统断言

### AC-5: 全局始终至多一个备份/验证/上传
- **Type**: `rule`
- **Given**: 多个计划存在
- **When**: 一个备份运行中（含其记录待写/清理窗口），并发或串行发起其他计划的 scheduled/manual 备份、check、upload
- **Then**: 第二者得到 ErrBackupRunning 或 ErrBusy（API 409）；数据库 running 部分唯一索引保证跨重启也不出现两个 running；验证/上传/清理结束后槽释放
- **Pass Condition**: 既有单槽测试继续通过，新增跨计划用例通过
- **Evidence**: `go test ./internal/backups/...`

### AC-6: 调度按到期时间与稳定次序公平选择，失败不饿死他人
- **Type**: `rule`
- **Given**: 使用 fake clock 的服务，存在 A、B、C 三个 enabled 计划，构造三者均已到期（或到期时间相同），并令 A 的目标不可用使其快速失败
- **When**: 推进调度器
- **Then**: 启动顺序严格按 (due, created_at, name)；A 失败被记录在 A 名下后，B、C 仍各自启动一次；A 在其下一个 interval 到达前不再被调度；各计划每轮至多补跑一次；手动 run 不影响到期次序
- **Pass Condition**: 确定性测试断言启动计划序列与各 run 的 plan 归属
- **Evidence**: `go test ./internal/backups/... -run Plan` 输出

### AC-7: run 记录计划身份并冻结启动时配置
- **Type**: `rule`
- **Given**: 计划 P 配置为目录 D1、verify=on、keep=2，一个 run 已启动
- **When**: run 运行中将 P 改为 D2、verify=off、keep=9（或禁用、删除——删除应被 AC-3 拒绝）
- **Then**: 当前 run 写入 D1、按启动快照做验证、按 keep=2 只在 P 内清理；run 记录的 plan=P；下一次 run 使用新配置
- **Pass Condition**: 用例检查实际写入目录、验证执行与否、删除候选集合
- **Evidence**: `go test` 用例输出与目录内容断言

### AC-8: 空间估算与旧备份清理按计划隔离，同目录不互删
- **Type**: `rule`
- **Given**: 计划 P、Q 使用同一 destination 目录并各有多次成功备份（另构造 P 的失败验证备份）
- **When**: 各自再完成一次成功 run
- **Then**: P 的清理只删 P 超出其 keep 的备份与 P 的旧失败验证备份；Q 的备份一个不删，反之亦然；非 OwnGit 文件不动；checkRoom 只以本计划最新备份尺寸估算
- **Pass Condition**: 用例枚举目录内容并逐目录断言归属与去留
- **Evidence**: `go test ./internal/backups/...` 隔离用例输出

### AC-9: 单目标不可用只失败该计划，其他到期计划照跑，修复后可手动重试
- **Type**: `rule`
- **Given**: A 的目标目录不可用（如不存在且无法创建/挂载点缺失），B 同时到期且可用
- **When**: 调度器运行
- **Then**: A 产生一条 failed run（plan=A，消息为该目录原因）；B 的 run 成功；修复 A 的目录后 `POST /plans/A/runs`（CLI `backup now --plan A`）成功
- **Pass Condition**: 用例断言两条 run 归属与状态，以及重试结果
- **Evidence**: `go test` 用例输出

### AC-10: 服务中途停止后重启，中断归原计划，他人不算已执行
- **Type**: `rule`
- **Given**: A 的 scheduled run 运行中，B 尚未运行
- **When**: 停止服务（模拟 ctx 取消/进程结束）后重启
- **Then**: A 的 run 为 interrupted 且 plan=A；没有任何属于 B 的 run 被创建或标记；A、B 的下次到期仅按各自 scheduled runs 计算；重启只对确已到期的计划各补一次；A 可立即手动重试
- **Pass Condition**: 用例断言中断记录归属、run 总数与各计划 next_run
- **Evidence**: `go test` 重启/中断用例输出

### AC-11: 兼容的状态视图与 summary 聚合
- **Type**: `rule`
- **Given**: 含多个计划（或零计划、仅 default）的状态库
- **When**: 调用 `/api/v1/backups`、`/runs[?plan=]`、`/summary`
- **Then**: 顶层旧字段保持且锚定 default；新增 plans 数组按稳定次序给出每计划 schedule/running/last_run/last_verified/next_run/restore_limit；run 项含 plan 字段并可按计划过滤；summary：零计划 not_configured、任一启用 on 否则 off、last_run/last_verified/next_run 跨计划按定义聚合
- **Pass Condition**: JSON 契约用例通过（含未配置与多计划两组）
- **Evidence**: `go test ./internal/server/... ./internal/backups/...`

### AC-12: 机器本地数据不进入离线备份
- **Type**: `rule`
- **Given**: 含 backup_plans 与 backup_runs 数据的状态库
- **When**: 生成离线备份并恢复到新状态目录
- **Then**: 备份集与恢复后的库不含任何计划与运行历史（沿用机器本地数据清单，加入新表名断言）
- **Pass Condition**: 便携快照/恢复测试通过
- **Evidence**: `go test ./internal/recovery/... ./internal/state/... -run Portable`

### AC-13: 文档、双语言与界面可达性同步
- **Type**: `rubric`
- **Dimension**: 用户界面与文档完整性
- **Scale**: 1-5
- **Anchors**: 1 = 仅功能可用，文案/文档/可达性缺失；3 = 英文文档与基本键盘操作齐备但韩语或外观/焦点处理有明显缺口；5 = BACKUPS 双语文档与 CHANGELOG 准确描述多计划/--plan/plan 子命令/隔离与删除语义，仪表盘新文案 EN+KO 齐备，Light/Dark/System 下布局与焦点可见、可纯键盘完成选择/新建/删除/立即备份，状态不只靠颜色
- **Pass Threshold**: >= 4
- **Evidence**: 文档 diff、messages 目录条目、浏览器键盘走查与三种外观截图/DOM 断言
