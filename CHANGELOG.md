# Changelog

All notable changes to OwnGit are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

### Added

- Open pull requests can now merge automatically after their checks pass. Enable the intent from the pull request page, with `owngit pr auto-merge --number N`, or with the owner API (`POST /api/v1/repositories/{r}/pull-requests/{n}/auto-merge`); cancel it with `--cancel` or the `.../auto-merge/cancel` endpoint. OwnGit merges only once the latest evidence of the current source commit passed on a clean worktree with the committed check configuration and the current pair merges without a conflict; missing, running, failed, stale, unreadable or conflicting states are shown as explicit waiting or blocked reasons and never merge. Branch moves re-evaluate the current pair, cancels and closes have a deterministic order against a background merge, pending intents survive restarts and backup restore, and repositories without automatic merging keep the advisory-checks manual merge behavior.

## [1.1.7] - 2026-10-09

This release fixes security problems rated Medium and Low by building OwnGit with Go 1.27.2. It has no other changes. Upgrading is recommended.

**Upgrading:**

- On macOS 27, a menu bar manager such as Hidden Bar can still hide the OwnGit icon when OwnGit.app runs from outside `/Applications`, as it does after the installers or Homebrew. Keep the menu bar manager expanded to see the icon; a fix is planned for 1.1.8.

### Security

- Medium: anyone who can reach OwnGit's web address, including its public share address while that is on, could make it spend too much processor time without signing in, and could make it use too much memory while access does not need the shared password. Affects 1.1.6 and earlier; upgrade.
- Low: in rare cases on Windows, OwnGit could create a folder outside its own folders, and an `owngit` command could use too much processor time when the server it connects to over HTTPS misbehaves. Affects 1.1.6 and earlier; upgrade.

## [1.1.6] - 2026-10-08

This release fixes security problems rated Low, lowers memory use, and makes the dashboard and automatic checks faster with many repositories or branches. Upgrading is recommended.

**Upgrading:**

- At every start, OwnGit now makes its state directory private to its own account and logs each change, so access that other accounts had to it is removed.
- On macOS 27, a menu bar manager such as Hidden Bar can still hide the OwnGit icon when OwnGit.app runs from outside `/Applications`, as it does after the installers or Homebrew. Keep the menu bar manager expanded to see the icon; a fix is planned for 1.1.7.

**Changes for scripts:**

- `owngit restore --json` lists malformed Git objects found in the restored repositories under `object_warnings`, with `id` and `message` for each repository. The field is left out when there are none.
- When the server could not finish checking the administrator password, the error details include `operation_started: false`. Status, code and message are unchanged.

### Changed

- An idle server uses about half the memory: 12.3 MiB of Go heap instead of 24.7 MiB, measured on macOS with no repositories.
- An import looks for large-file pointers while it reads the list of fetched objects, instead of holding the whole list in memory first. In a measured repository with many objects and long paths, Go heap growth fell from 75.2 MiB to 5.3 MiB.
- On Linux, the number of password checks that run at once follows the host's memory: one at a time at 256 MiB or less, two at 512 MiB, three at 768 MiB and four above. Each check uses 64 MiB, and new passwords in Settings, share links and setup now count toward the same limit.
- The dashboard, its search and the all-repository activity page open faster with many repositories. With 1,000 repositories on Linux, the dashboard went from 122.6 to 63.5 ms, search from 79.2 to 23.2 ms and activity from 114.5 to 57.7 ms.
- Automatic checks remember up to 10,000 branches per repository instead of 64, so unchanged branches are skipped. With 10,000 branches on a Raspberry Pi 5, a scan with no changes fell from 73 seconds to 0.4 seconds.
- A scheduled import that waits for a busy import slot starts as soon as a slot is free, instead of at the scheduler's next regular check.
- Messages about a changed repository folder now give both ways to continue: put the original folder back, or restart OwnGit to use the folder now in its place. Renaming a repository and changing its import settings still work while one repository's folder is missing.
- `owngit restore` checks the Git objects of each restored repository and prints a warning with the repair steps for each repository that holds malformed objects. The backup is still restored.
- On small Linux hosts, a file that cannot be compared within the size or memory limits is shown as "Text comparison unavailable" instead of too large.
- On Windows, the state directory and its parent folders cannot be renamed or moved while the server or a command has the state open. Stop OwnGit before you move them.
- An external check runner is refused before it downloads anything when the check source holds a file the server cannot read within its limits. The status and code are unchanged, and the message names the limits.

### Fixed

- A language, appearance or list order chosen at the HTTPS address no longer overrides the choice made at a plain HTTP address of the same host, and the same applies to a share link password. Saved choices carry over after the upgrade.
- `owngit runner-credential issue` and `owngit helper-credential create`, repeated with the same creation ID, no longer revoke the earlier credential when the server could not finish the password check.
- The dashboard and all-repository activity show a repository whose folder was replaced while OwnGit runs as unreadable, as its own page already did, instead of the last cached listing.
- On small Linux hosts, files that OwnGit refused before it repacked a repository are checked again afterwards, so files that are now readable are shown.
- On Linux, a check whose cleanup cannot read the list of running processes ends as `error` with the reason in `cleanup_error`, instead of passing while its programs may keep running.
- On a busy Windows host, a cancelled check no longer ends as an error because confirming that its programs exited took too long.
- At start, OwnGit leaves correct Git hooks alone and replaces a changed hook in one step, so Git never runs a hook that is only partly written.
- On Windows, OwnGit starts with a state directory whose permissions your account may change but whose owner it may not, such as a folder directly under `C:\`. 1.1.5 stopped with "Access is denied".

### Security

- Low: on a computer shared with other local accounts, OwnGit now makes its state directory and the files it manages there private to its own account at every start, on Linux, macOS and Windows. Affects 1.1.5 and earlier; upgrade.
- Low: on a computer shared with other local accounts, `owngit doctor`, `owngit service status` and the menu bar or panel icon now confirm a running OwnGit by the same proof that `owngit health` checks, and show any other answer as not confirmed, with the reason. Affects 1.1.5 and earlier; upgrade.
- Low: on a computer shared with other local accounts, OwnGit now checks each repository's own folder before pushes, pull request changes, imports, restores, maintenance, deletion and backups. A folder put in its place while OwnGit runs is refused (HTTP 409 for Git and the API), and a backup fails instead of saving only part of the repositories. Affects 1.1.5 and earlier; upgrade.

## [1.1.5] - 2026-10-07

This release fixes security problems rated Medium and Low, bounds waits that could last without end, and lowers memory use on small Linux hosts and with large repositories. Upgrading is recommended.

**Upgrading:**

- Every browser signed in at the HTTPS address is signed out once. A sign-in at a plain HTTP address ends once, the first time that browser opens the HTTPS address.
- Run `owngit service install` once after the upgrade, so that the service restarts and gets the longer 70-second stop wait; until OwnGit restarts, `owngit health` reports `it published no health key; restart OwnGit`. The one-line installers do this for you, and with Homebrew, run `brew services restart owngit` after `brew upgrade owngit`.
- With plain Docker, use `docker run --stop-timeout 70` or `docker stop -t 70`; `compose.yaml` already sets it.
- Pushes of malformed Git data that earlier versions accepted are now refused, with the reason on `remote: error:` lines.
- On Linux, programs that a check starts are stopped when the check ends. Start long-lived programs outside OwnGit.
- On macOS 27, a menu bar manager such as Hidden Bar can hide the OwnGit icon when OwnGit.app runs from outside `/Applications`, as it does after the installers or Homebrew. Keep the menu bar manager expanded to see the icon; a fix is planned for 1.1.7.

**Changes for scripts:**

- Pull request diffs (API, `owngit pr diff`, MCP `pull_request_diff`) and the restore preview mark a file too large to compare with `too_large: true`. Such a file, and a file whose line counts were not read, has no `additions` or `deletions`, and `binary` is `false`; a diff missing only such files has `reason` `too_large`.
- An archive refused with 409 has the code `archive_repeated_path`, `archive_too_many_files`, `archive_deep_chain` or `archive_memory` instead of `archive_failed`.
- The pull request list (API, `owngit pr list`, MCP `pull_request_list`), a repository's task list (`GET /api/v1/repositories/ID/tasks`, `owngit check task list`, MCP `check_task_list`) and `GET /api/v1/tasks/NAME` return pages of 50 (at most 100), newest first, with `next`. The pull request list takes `state`, `limit` and `before`, and no longer fails with `result_too_large`.
- A push or fetch to a busy repository gets 503 after the slot wait (90 seconds by default), and an import change on a busy repository gets 409. On Linux, a file page or raw download gets 503 with `Retry-After` when the server is busy.
- A clone or fetch request larger than 10 MiB gets 413 instead of 502.
- A configured check rerun is refused with `check_workflow_changed` when the check file changed.
- `owngit check run` exits with 128 plus the signal number (130 for Ctrl-C) when stopped, adds `worktree_note` when the working copy state is `unknown`, and stops with `revision_unavailable` when it cannot read the committed check file in 30 seconds.
- In `owngit tailscale status --json`, `can_turn_off` says whether the dashboard offers turning sharing off.

### Added

- `owngit import configure` can change an import's source address, mode and consents, and attach a source to a repository that has none, as the dashboard can.

### Changed

- OwnGit is now licensed under GPL-3.0-or-later instead of MIT. If you distribute OwnGit or a modified version, you must offer its source code under the same license; releases 1.1.4 and earlier stay MIT.
- Setup, the Access tab and the guides now say plainly what open access lets anyone on the network do, and what still needs the administrator password.
- A browser keeps separate sign-ins at the HTTPS address and at a plain HTTP address of the same host. Signing out at the HTTPS address ends both.
- Small Linux hosts use memory-aware limits for Go, Git and concurrency, and refuse oversized file reads before Git starts.
- A request to a repository busy with another Git operation no longer waits without limit, and a client that gives up leaves no work running on the server.
- With many repositories, the dashboard opens faster after the first view.
- Commits and pull requests with many changed files show 400 files per page, and a large file diff opens alone in pages of 10,000 lines.
- All commits shows 100 commits per page with Older and Newer links, so older commits are reachable.
- The pull request page shows open pull requests by default, newest first, with links to closed, merged and all.
- A refused archive says why on the archive page, in English or Korean.
- Styles, scripts and SVG images are sent compressed, and static files are checked for changes instead of downloaded again.
- Large restores apply much faster. Restoring selected files with none ticked points to the Files field.
- Repository maintenance runs only when a repository has new work, so restarts and unchanged pushes no longer repack every repository, and it no longer skips nights.
- Automatic checks skip repositories whose refs did not change, with a full scan at least every 10 minutes.
- Configured checks: every matching branch update in a push gets its own job, and branch heads that never got a job are queued when push checks are turned on and at each start.
- Configured checks run oldest first across repositories, and a rerun uses the limits the check file asks for, up to the current maximums.
- A long check log keeps the beginning and the end of the output, so the lines that explain a failure stay visible.
- The Checks tab lists tasks in pages of 50, newest first.
- `owngit check run` no longer hangs on a stuck Git filter. Closing the terminal stops a run; a run started with `nohup` keeps running.
- Scheduled imports wait for a free import slot instead of failing when every slot is busy.
- When the repository folder is slow at startup, OwnGit logs what it waits for, and a pull request page that times out says so.
- Tray notifications: on Windows, notifications that arrive together appear one after another, or as one summary for more than three. On Linux, a slow notification service no longer shows a notification twice.
- The POSIX and Proxmox installers give up on a stalled download after about three minutes at most, instead of waiting forever.
- The English and Korean guides are shorter, with separate guides for repositories and imports and for backup and restore.

### Fixed

- Small Linux hosts no longer run out of memory when showing, downloading or comparing a file that needs much memory to rebuild. OwnGit says the file is too large instead; a clone of a single branch can still need that memory.
- Git at a renamed repository's old address works during the 90 days, also with the shared password, and tells you the new address.
- Signing in at a plain HTTP address works after signing in at the HTTPS address of the same host.
- A stop now finishes within what service managers allow. A stop during a Git transfer can take up to about 45 seconds, and the next start records what was interrupted.
- With Homebrew, `owngit service install` after an upgrade no longer refuses the previous version's state.
- The restore command on the Storage & recovery tab works as shown, without `sudo` on Linux and after an archive or source install.
- After a restore behind a reverse proxy, the restore steps and error pages name the network settings to save before starting OwnGit.
- A full disk at the end of a backup no longer blocks later backups until a restart.
- Repeated backup verification failures no longer fill the backup destination; OwnGit keeps only the newest failed backup.
- `owngit health` works when the state directory is read-only.
- A log file that cannot rotate no longer stops logging or keeps OwnGit from starting.
- Configured checks no longer end as an error when a runner's job lease runs out, and a branch with a very long name no longer stops checks of later branches.
- `owngit check run` no longer waits without end for the committed check file in a partial clone whose remote does not answer.
- Branch names that Git accepts, such as `a./b`, work in pull requests, the initial branch and check branch patterns.
- OwnGit starts faster with many repositories.
- Turning tailnet sharing off in the dashboard works while Tailscale is stopped, signed out or starting.
- A Tailscale sharing change started while another one runs no longer waits without end.
- A grouped push notification opens the repository when the latest push deleted its branch or tag.
- On Windows, a notification no longer stays on screen past the display time in accessibility settings, and a newer one replaces it.
- Linux: the OwnGit icon gives up after 30 seconds when the desktop panel does not answer, and its panel fits small screens.
- In Windows contrast themes and other forced colours, switches, the activity graph, selections, tabs and folding triangles stay visible.
- The focus ring of a diff file is no longer cut off, and screen readers say whether each diff line was added or removed.
- Storage and recovery no longer shows unsaved changes when backups are not configured.
- Korean: revoking a token is now 폐기, distinct from 취소, and backup and upload messages appear in Korean.

### Security

- Medium: share link holders could download files from kept history and deleted branches, and Git clients could fetch kept history. Affects 1.1.4 and earlier; upgrade, and replace any secret that kept history held if share link holders could have reached it.
- Low: on a computer shared with other local accounts, another account could replace OwnGit's password or token files on Linux and other Unix systems, read its logs on macOS, make `owngit health` report a server that is not OwnGit, or make OwnGit write into a repository folder it had not claimed. Affects 1.1.4 and earlier; upgrade.
- Low: in some network setups, the limit on wrong administrator passwords could be bypassed. Affects 1.1.4 and earlier; upgrade.
- Low: a push could store Git data that OwnGit's pages and archives showed differently from Git. Affects 1.1.4 and earlier; upgrade. Repositories that already hold such data keep it, and pages refuse to show it.
- Low: on Linux, programs started by a check could keep running after the check ended. Affects 1.1.4 and earlier; upgrade.

## [1.1.4] - 2026-10-04

This release fixes security problems rated Medium and Low, and bugs that could make every later backup fail. Upgrading is recommended.

**Upgrading:** on macOS, OwnGit makes its server log private whenever it opens it, and if it cannot, it does not start and names the `chmod -N` command that fixes the file. An existing LaunchAgent picks up the new service settings after you run `owngit service install` again, and a Homebrew service after you upgrade and run `brew services restart owngit`; [Log files on macOS](docs/OPERATIONS.md#log-files-on-macos) covers the output file Homebrew writes. 1.1.3 restores backups made by 1.1.4, except a backup that holds a branch or tag name with a Unicode space character, a record left by a refused or stopped merge that differs from the pull request's later merge, or check records in a time order 1.1.3 refuses; such a backup needs 1.1.4 or later ([Backup versions](docs/OPERATIONS.md#backup-versions)).

**Changes for scripts:** a query string with a broken percent escape or a stray semicolon gets 400 `invalid_request`. Repository details from the API, `owngit repo show` and MCP add `default_branch_error` when the branches could not be read, and changing the default branch or creating a pull request with a name that matches two branches fails with 422 `ambiguous_branch`. Creating a repository can fail with the new `repository_storage_in_use` or `repository_create_kept`. When an import's first run cannot create its folder for a reason other than an existing name, it fails with 503 `repository_create_failed` instead of 409 `repository_taken`. `owngit backup --output --json` adds `warnings` when the backup found alias branches.

### Changed

- Long files, single-file diffs and folders are shown in pages of at most 10,000 lines or 1,000 entries, with page links above and below that stay on the same commit. A line address beyond the end of a file says so and links to the first page.
- A slow page keeps arriving for up to 10 minutes while data moves, and a page that did not arrive in full keeps showing "Receiving the page".
- Opening an unchanged repository again reuses what OwnGit already read while it is still in memory, and the dashboard counts a repository's activity in the background after a push.
- Backup results name alias branches, which a backup stores as ordinary branches, with the command that reconnects each after a restore.
- `owngit doctor` and the Checkup in Settings report repository folders that other local accounts can reach and change, with a repair command, and a repository whose managed hooks are a link or belong to another account. Setup continues with a warning when other accounts can reach and change the repository folder you chose.
- When OwnGit cannot tell where a request through a trusted proxy came from, setup warns and, by default, selects the shared password for general access and does not keep the address you used.
- On macOS, the OwnGit service runs with interactive priority, so macOS does not slow it down as background work.
- On macOS, the menu bar panel uses the standard macOS text size. Its settings offer Large and Larger sizes, also with ⌘+ and ⌘- while the panel is open, and a panel taller than the screen scrolls.
- OwnGit no longer refreshes a repository's managed hooks through a linked `hooks` folder or hook file. Such a repository stays unavailable until you move the link out of its folder.

### Fixed

- A pull request merge that was refused or stopped, or a computer clock set back while checks ran, no longer makes every later backup fail. Existing installations back up again without any repair.
- Backups restore branch and tag names that contain Unicode space characters, such as a no-break space, exactly.
- After OwnGit stopped while an import was setting the default branch, pushes to that branch are no longer blocked when OwnGit can prove the import stopped and the repository's folders are private to the OwnGit account; otherwise the manual steps are shown. The import keeps following the source's default branch when OwnGit can prove it set that branch; otherwise it leaves the branch as it is and reports a difference once the source's default branch moves.
- On macOS, file and branch names stored with decomposed Unicode, such as Korean file names created on Linux, keep their exact bytes, so their pages, diffs and backups work.
- Choosing a branch in Settings or on the New pull request page uses exactly the branch you picked, even when another branch is named after its full name, such as `x` and `refs/heads/x`. A name that could mean two branches is refused with advice on how to choose one.
- Two installations or two requests can no longer create a repository in the same folder, and a client that disconnects after the folder is created no longer stops OwnGit from recording the new repository. When recording still fails, OwnGit keeps the folder instead of deleting it; check it before you try that name again.
- The command shown to bring back a deleted repository that had been renamed uses its current name.
- `owngit health` succeeds only when this installation's OwnGit is running and answers.
- On Linux, uninstalling no longer reports success and removes the service when systemd could not stop it.
- A partial settings save changes only the fields it names and no longer undoes another save made at the same time.
- Restoring selected files from an older commit never deletes files that were not selected.
- Markdown Preview and Source stay on the commit you are viewing.
- On macOS, Tab moves through the menu bar panel's controls every time the panel opens, also after the panel changed while it was open.
- When this computer's loopback address, such as `127.0.0.1`, is a trusted proxy, the OwnGit icon works, and a push that connects directly over loopback, not forwarded for another client, is no longer announced as coming from another computer. A browser that opens a dashboard page directly over plain HTTP, using the name of a confirmed working HTTPS address, moves to that address even when its connection address is a trusted proxy.
- On Windows, the command OwnGit prints to make a password, token or repository folder private also fixes its owner, works in Windows PowerShell 5.1 and PowerShell 7, and names any item it leaves unchanged or cannot repair.

### Security

- Medium: on macOS and Windows, new repository folders could inherit access, including the right to change them, from a repository folder that other local accounts can access. Affects 1.1.3 and earlier; new folders are now private to the OwnGit account, and `owngit doctor` names existing folders to fix.
- Low: on macOS, other local accounts could read OwnGit's password, token and import secret files or its logs when access lists allowed it. Affects 1.1.3 and earlier; OwnGit now refuses such secret files with the command that fixes them and keeps its server log private.
- Low: behind two or more trusted proxies, one visitor's failed sign-ins could lock out others, and behind any trusted proxy that did not say where a request came from, that request could be treated as coming from this computer. Affects 1.1.3 and earlier.

## [1.1.3] - 2026-10-01

This release fixes security problems rated Medium and Low. Upgrading is recommended.

**Upgrading:** the first start of 1.1.3 upgrades the state database from schema 15 to 16. Before it does, OwnGit makes an offline backup of the state in a folder beside the state directory, such as `~/.config/owngit-backups`, and the log names the command that restores it. `owngit upgrade-backup off` turns this backup off. Earlier versions refuse the upgraded database, so to go back, restore that backup with the earlier version. OwnGit now refuses to open or upgrade a state database whose tables, indexes, triggers or views were changed by hand; undo the change, or restore a backup with the OwnGit version that made it. Backups are still written in format 10, which 1.0.3 to 1.1.2 restore, unless they hold records that only the new format 11 can hold, such as a repository's own kept history choice, or more records than format 10 allows. The state directory and every folder on the way to it must now be on a local disk, so a state directory reached through a network share is refused. Backups and restores write only into folders that no other account can change; when OwnGit refuses a folder, it names it and prints the command that fixes it when there is one.

Pushes and imports now refuse a branch or tag whose name, or any folder in it, matches another ref except for letter case, Unicode encoding or letters that some file systems treat as equal. A repository that already holds two such names, for example `main` and `Main`, refuses pushes that create or update either one. Delete one of them with `git push origin --delete NAME`. The other ref keeps its value, the default branch cannot be deleted this way under any spelling, and the deleted commit stays in kept history when kept history is on.

**Changes for scripts:** JSON from the API, the command line and MCP writes Unicode direction controls as `\uXXXX` escapes; the values are unchanged. API requests and MCP arguments that are not valid UTF-8, or that escape half of a UTF-16 surrogate pair, are refused with `invalid_json` or `invalid_arguments` instead of being stored with replacement characters. The administrator API, including the helper credential, check and import owner routes, needs the administrator password in the Basic header for reads as well as changes, as the command line sends it. A source with more refs than an import accepts fails with `too_many_refs`. An interrupted `owngit restore` says that nothing was restored and exits with 130. When the command line's own time limit ends a request, it reports `connection_failed`. A script that runs `owngit network set` with an address other computers reach over plain HTTP must add `--accept-insecure-http` once.

### Added

- One-line installers for Linux, macOS and Windows download the latest or a named release, check it against `SHA256SUMS`, install `owngit` and run it as a service.
- `owngit update` prints the command that updates OwnGit the way it was installed, and the new-release notice shows it to a confirmed administrator. `owngit uninstall` removes the service and names the command that removes the program.
- A container image for Linux on x64 and ARM64, `ghcr.io/juliankang4/owngit`, comes with a Compose file and runs as a non-root account with its data in one volume. It updates with `docker compose pull && docker compose up -d`.
- A helper script for Proxmox VE hosts, `https://owngit.app/proxmox.sh`, creates an unprivileged Debian 13 container and installs OwnGit in it with the one-line installer. It can keep the repositories in a folder of the host, and it never changes an existing container.
- An OwnGit icon in the Windows notification area, the macOS menu bar and the Linux desktop panel (with `gjs` and GTK 4) shows whether OwnGit is running or needs attention, the clone address and the latest pushes, and opens the dashboard. `owngit service install` sets it up to open at sign-in, and `owngit tray on` and `off` or a switch in Settings show or hide it without stopping OwnGit.
- `owngit tray read` prints what the icon's panel shows and `owngit tray open` opens the dashboard, so other desktop panels, such as the Omarchy bar widget in `integrations/omarchy`, can use them.
- `owngit doctor` and a Checkup card in Settings list problems such as a stopped server, Windows folders that the Administrators group owns, or a firewall that keeps other devices out, with a repair command or advice for each.
- Kept history can be turned off for the whole server or for one repository, and a repository's default branch can be protected from rewrites and deletion. Both are in Settings, the API, `owngit settings set` and `owngit repo settings`.
- `owngit repo kept-history`, `owngit repo restore preview` and `apply`, the matching API routes and MCP tools list kept history and restore files. They use the same preview, and the same check that the branch has not moved, as the restore pages.
- The pull request page, `owngit pr mergeability`, the API and the MCP tool `pull_request_mergeability` answer on request whether a pull request can merge now, and name the conflicting paths when it cannot. The check writes nothing.
- A pull request can have a Markdown description, its title and description can be edited, and a review can carry a note. `owngit pr edit` and the MCP tool `pull_request_edit` do the same, and an edit based on an outdated version is refused with `stale_edit`.
- `owngit backup verify` rehearses a restore in a temporary folder and reports whether the backup restores, without changing it or the running OwnGit. `owngit restore --verify` restores only a verified backup.
- Settings has five tabs, General, Access, Network, Repositories, and Storage & recovery, with a Save for each part, and asks before you leave it with unsaved changes.
- New settings choose how long a sign-in with the shared password lasts, the branch new repositories start on, the Git transfer size and time limits, and how long raw check logs are kept. `owngit settings show` and `owngit settings set` read and change them.
- The Access tab chooses how often the dashboard asks for the administrator password, from every time to once in 30 days, or never. By default it asks again after 30 minutes.
- More settings choose whether deleting a repository asks for its name, the login attempt limits, whether a link from another site keeps you signed in, and the Tailscale HTTPS port, and can replace what another service serves on that port. Each keeps the earlier behavior by default, and none is in backups.
- The repository lists share one row layout, and a Sort control orders them by latest update or by name, remembered in each browser.
- Scheduled backups run every 12 hours, every day or every week while OwnGit serves, verify each backup and keep the newest copies (7 by default). `owngit backup now`, `status`, `runs` and `schedule`, the owner API under `/api/v1/backups` and a `backup_status` summary for coding tools work with them.
- A Backups group in Settings, Storage & recovery sets up scheduled backups and shows the last and next ones with a Back up now button. Each backup can be verified again, downloaded as a tar file, or restored with the command shown for it, and `owngit backup check`, `download` and `upload` do the same from the command line.
- An uploaded backup file is verified first and shows the command that restores it. It is kept until OwnGit starts again or for at most 24 hours, so run that command, with OwnGit stopped, before you start OwnGit again.
- The OwnGit icon shows desktop notifications for pushes, new pull requests, failed checks, imports or backups that did not finish, and a new OwnGit version; each kind can be turned off in the icon's panel or with `owngit tray notifications`. On Windows they are notification area balloons, which Windows does not show or keep while Do not disturb is on.
- Each import source can allow plain HTTP, follow redirects to the same origin or one approved origin, allow a special-purpose destination address, and change its size and time limits. Every choice is off or at the earlier value by default, shows a warning, and is also in the import API, `owngit import add` and the new `owngit import configure`.
- An import can bring in ref namespaces beyond branches and tags, such as `refs/notes/`. Before a refresh changes anything, the owner sees its effect on each ref and chooses whether it follows deletions at the source and whether it overwrites branches that diverged in OwnGit; the protected default branch is never rewritten.
- The setup page has a Choose button beside the repository folder, which browses the folders of the computer running OwnGit and can create a new one. Only the browser doing setup can use it, and only until setup is finished.
- Settings choose how many Git transfers run at once and how long they may wait, how much a page shows when browsing files, diffs and comparisons, and when repository maintenance runs. Each keeps the earlier behavior by default.
- Unused object cleanup, off by default, removes objects that no ref and no kept history reach once they are older than a grace period of at least 2 days (14 by default), inside the maintenance window. A check whose commit it removed can no longer be run again.
- An administrator can let one repository accept pushes to other ref namespaces, such as Git notes under `refs/notes/`. Refs there have no kept history or protection.
- A repository can be renamed on its Settings tab, with `owngit repo rename` or through the API, keeping its history, pull requests and checks. For 90 days the old address still leads to it for pages, the API and Git.
- Read-only share links open one repository, for browsing or also cloning, to someone without the shared password. The owner creates them on the repository's Share links page, with `owngit repo share` or through the API, with an optional expiry and extra password, and can revoke them.
- Share links can be published on a separate public address, such as one reached through Tailscale Funnel or a reverse proxy, while the rest of OwnGit stays private. It is off by default and set in Settings, Network or with `owngit network set --public-share-listen` and `--public-share-url`.
- `owngit activity` and the MCP tool `activity` list recent commits as the Activity page does, and `owngit tasks` lists check tasks as the Checks tab does. The Activity page and these commands show the newest 1,000 commits and say when the list is cut.
- A Coding tools page shows the commands that connect coding tools to this server through the skill and MCP, with Copy buttons, and lists recent tasks.
- Owner tasks that needed the dashboard also work from the command line with `--json`: `owngit settings access`, `settings admin-password`, `settings confirmation`, `settings set --update-check`, `repo default-branch` and `repo delete`. New passwords come only from a file only you can read or from a hidden prompt.
- The import commands, `owngit backup --output` and `owngit restore` print JSON with `--json`, including failures with a stable error code. Without `--json` their output is unchanged.
- The Automatic checks page lists leftover check containers that OwnGit could not remove, and forgets one once you confirm it is gone.
- Configured checks that run in containers have new options, each off by default with a warning, such as an image tag instead of a digest, downloading a missing image and an existing named Docker network. Save and turn on checks, or `owngit check-policy set --enable`, saves a policy and turns checks on for exactly that policy in one confirmed step.
- Check ceilings in Settings, Repositories set how much automatic checks may use, such as run time, output, memory and scratch space, with the earlier limits as defaults. The settings API and `owngit settings set --check-*` change them too.

### Changed

- Backups can hold up to 1 GiB of OwnGit records, not counting the repositories, so large check, pull request and import histories fit.
- Imports ask the source for Git protocol v2, so refs that an import does not use, such as pull request refs, no longer count toward the 50,000-ref limit of such a source.
- Imports accept old commits and tags with a malformed time zone or a missing tag date, and trees with zero-padded folder modes. They refuse commits whose dates OwnGit cannot show.
- A commit whose time zone offset is 24 hours or more is shown in UTC instead of breaking the page, and a repository whose latest commit cannot be read stays readable.
- Merge and restore commits that OwnGit writes carry this computer's UTC offset instead of +0000.
- A dashboard page opened over plain HTTP by the name of a working HTTPS address, from Tailscale sharing or a trusted proxy, moves to that HTTPS address.
- OwnGit reads Tailscale's status from Tailscale's own service instead of running the `tailscale` command.
- `owngit service install` refuses an `owngit` that another account could replace, for example one in `D:\tools` on Windows. Move `owngit` to a folder only you can change, or install it with the one-line installer.
- A push that updates a symbolic ref other than `HEAD`, such as a branch made on the server as another name for a branch, is refused with a message to push to the ref it points to.
- `owngit network set` refuses an address that other computers reach over plain HTTP until you accept plain HTTP with `--accept-insecure-http`, as the dashboard does. Addresses on this computer need nothing.
- An idle server with many repositories uses much less CPU, and the first dashboard after a restart runs fewer Git processes for each repository's activity.
- A check that prints more than its output limit is stopped at once and recorded as incomplete.
- After a restore, `owngit restore` lists everything the backup did not bring back, such as sessions, network settings, tokens, import credentials and share links, and says where to set each up again.
- `owngit serve` opens no browser and asks nothing in a session that nobody watches.
- Backup and restore create missing parent folders of their targets.
- A check job's finish time is the time OwnGit recorded it; the external runner's own time stays on the attempt.

### Fixed

- A Git client that is locked out after wrong shared passwords gets HTTP 429 instead of 401, so it keeps the password its credential helper saved. An upload that stops sending gets 408 instead of 502.
- A form larger than 1 MiB gets a page that says nothing was saved, instead of a bare error.
- An import refuses a repository name that Windows reserves, such as `CON`, before it starts.
- Restoring or verifying a backup of a SHA-256 repository works.
- One commit whose date Git prints in a form OwnGit cannot read no longer hides the dashboard's activity or the repository's pages. OwnGit names that commit and shows the rest.
- Backups, `owngit backup verify` and restores accept check records from an external runner whose clock was behind OwnGit's, instead of refusing the whole state.
- On macOS, backups to exFAT, FAT16 and FAT32 volumes complete. A repository restore onto a volume that Git cannot use correctly is refused before any work and names the file system, while the state folder restores onto FAT16 and FAT32.

### Security

- Medium: on a computer shared with other local accounts, another account could make OwnGit change files or settings, when OwnGit ran as root, used a folder or program that account can change, or reached a Tailscale service that account controls. Affects 1.1.2 and earlier; OwnGit now refuses these unsafe setups and names what to fix, and ordinary Tailscale use is unaffected.
- Low: signing out or changing a password did not always end every session it should, and the administrator API did not always require the administrator password. Affects 1.1.2 and earlier.
- Low: check logs and summaries could keep part of a credential that OwnGit hides in check output, when a check printed it. Affects 1.1.2 and earlier; if your checks printed such a credential, consider replacing it.
- Low: text from repository users, such as pull request titles, branch names and commit messages, could change how the names and labels beside it were shown. Affects 1.1.2 and earlier.

## [1.1.2] - 2026-09-28

This release fixes two low-severity security problems. Upgrading is recommended.

### Added

- The macOS binary in the release archive is signed with a Developer ID and notarized by Apple, so macOS runs it without the quarantine workaround.

### Changed

- The macOS and Homebrew services write their log to the rotating log file, and the same failure repeated within a minute is logged once with a count. Run `owngit service install` again, or restart the Homebrew service, to use the new log settings.
- An import failure that OwnGit cannot classify is reported with the code `unclassified` instead of `unsupported`.

### Fixed

- A password or settings change that fails to save no longer signs the browser out, and the page says what was and was not changed.
- A request that could not read OwnGit's own state answers that OwnGit is unavailable and logs the cause, instead of "not signed in", "not found", "invalid input" or an empty result.
- Tailscale sharing: a turn-on whose settings could not be saved is finished by the next turn-on or turn-off; a port change no longer rewrites or later removes an HTTPS address you made in Tailscale; when Tailscale hangs, the page answers in time and says so.
- A configured check that could not start is recorded as not run with the reason, instead of ending as ambiguous.
- The restore page keeps the file list and the selection when a preview fails, and says why.
- Scheduled imports keep running when other repositories are still being prepared.
- Offline backups keep configured-check histories that the backup check used to refuse.
- Very large check output no longer takes hundreds of megabytes of memory when it is cut for the log.

### Security

- Low: server log files could be read by other local accounts. They are now readable by their owner only. Affects 1.1.1 and earlier on Linux and macOS.
- Low: signing out could report success while the session stayed valid on the server. It now says that you are still signed in. Affects 1.1.1 and earlier.

## [1.1.1] - 2026-09-27

This release fixes security problems. Everyone should upgrade.

### Added

- `owngit service install` runs OwnGit in the background and starts it on its own: at boot on Linux, at sign-in on macOS, and on Windows at boot from an administrator account (one approval) or at sign-in from a standard account. `owngit service status`, `start`, `stop`, `restart` and `uninstall` manage it; `uninstall` keeps your data.
- On a computer without a screen, the first start listens on every address until setup is finished, and the setup link is printed in the terminal.
- When root already used OwnGit with its own state, `owngit service install` prints the commands that move that state into the service.
- `GET /healthz` and `owngit health` report whether the server answers.
- `owngit serve --log-file FILE` also writes the server log to a file.

### Changed

- Sharing on your tailnet uses port 8443 or 10000 when port 443 is already in use. `owngit tailscale on --https-port PORT` chooses the port.
- A page opened over the tailnet at this computer's Tailscale address says "Encrypted by Tailscale".
- `owngit setup-link` makes the link for the address the server listens on.
- `owngit network show` and `owngit tailscale status` work before the first start, and every failure under `--json` is a JSON error.
- On Windows, an SSH session counts as a computer without a screen, and commands that use the state directory drop administrator rights when started from an elevated terminal.

### Fixed

- On Windows, OwnGit started without a visible desktop no longer opens a browser.

### Security

- High: when OwnGit listened on a network address, another device could get past the allowed Host check. Affects 1.1.0 and earlier.
- Medium: `owngit runner` started as root could use a workspace folder that another local account controls. Affects 1.1.0 and earlier on Linux and macOS.
- Medium: a client without a password could hold connections open and make OwnGit unreachable. Affects 1.1.0 and earlier.
- Low: on macOS, private state files could stay readable by other local accounts through inherited access lists. Affects 1.1.0 and earlier.
- Low: a state directory whose parent folders another local account can change is now refused. If yours is refused after upgrading, run the command it prints or move the state directory.

## [1.1.0] - 2026-09-27

This release fixes a security problem in `owngit check run`: it no longer starts a clone's `core.fsmonitor` program or index hooks while it inspects the worktree. See Security below.

**Upgrading:** the state database stays at schema 15 and offline backups at version 10, as in 1.0.3, and OwnGit 1.1.0 upgrades the database of any earlier 1.0.x release directly. You can go back to 1.0.3 without restoring a backup. It ignores the listen address, base URL and trusted proxies saved with `owngit network set`, so start it with the options you used before. Host names allowed with `owngit network set --allowed-host` stay allowed under 1.0.3, because they are stored with the names `owngit approve-host` approves. Helper credential and runner token files written by 1.1.0 start with an `owngit-server:` line that 1.0.3 does not understand, and 1.0.3 then reports a connection failure; delete that first line, or create the file again with 1.0.3. On Windows, 1.0.3 also refuses a password file owned by the Administrators group, which an elevated PowerShell gives new files and 1.1.0 accepts; make the file again in a PowerShell that is not elevated. Upgrading from 1.0.3 and going back were tested on Linux, Windows, macOS and a Raspberry Pi. Before you upgrade, stop OwnGit and make a backup with `owngit backup`.

**Changes for scripts:** helper credential and runner token files written by 1.1.0 start with an `owngit-server:` line, so scripts that read them as a bare token must take the last line. `owngit pr`, `owngit check` and `owngit repo` read a missing `--server` or `--repository` from the clone's `origin` remote and fail with `origin_*` codes instead of `invalid_arguments`. Password and credential files must hold the secret on one line. `setup-link` and `reset-admin` refuse arguments they do not know. A gzip Git request body followed by more data is refused. If you set up Tailscale Serve for OwnGit by hand, `owngit tailscale on` refuses to take over that entry until you remove it. Details are below.

### Added

- Share OwnGit on your tailnet over HTTPS. In Settings or with `owngit tailscale on`, OwnGit sets up Tailscale Serve on this computer, saves the HTTPS address and name, and shows the `https://NAME.TAILNET.ts.net/` clone address. Turning it off removes only what OwnGit made. If HTTPS port 443 already serves something, including an entry for OwnGit that OwnGit did not make, turning on changes nothing and shows the command that clears the port. Before the address is served, OwnGit explains that the certificate puts the computer and tailnet names in a public certificate log. `owngit tailscale status|on|off` do the same from the command line, and changes made there apply at the next start. `owngit serve --tailscale PATH` and `owngit tailscale --tailscale PATH` use a `tailscale` command that OwnGit does not find on its own.
- Reverse proxies: `owngit network set --trusted-proxy ADDRESS` (or `serve --trusted-proxy`) names proxies whose `X-Forwarded-Proto`, `X-Forwarded-For` and `X-Forwarded-Host` OwnGit uses, so cookies are `Secure`, HTTPS forms pass the Origin check and each device behind the proxy has its own password lockout. Nothing is trusted by default. The operations guide has a "Behind a reverse proxy" section with tested settings for Caddy, nginx, Traefik and Nginx Proxy Manager, including the Nginx Proxy Manager line that makes it report each device's own address, and explains when HTTP/2 between Git and the proxy helps.
- The operations guide explains how to reach OwnGit over other private networks, such as NetBird, Headscale or plain WireGuard, including an HTTPS address through a reverse proxy on this computer. The steps were tested with self-hosted setups. Sharing from the Settings switch works only with Tailscale, which serves HTTPS with a certificate for this computer's tailnet name; the other networks named do not offer that on the computer.
- `owngit network show`, `set` and `reset` save the listen address, the address other devices use (base URL) and allowed names, so a server started without options, such as `brew services`, can be reached from other devices. Saved values apply at the next start; a `serve` option still overrides one for that run. `network reset` recovers from a saved value that locks you out.
- Settings has a Network card showing each of these values for the next start and for the running server, with the restart state. Changing them needs the administrator password.
- `owngit pr diff` and `GET /api/v1/repositories/{id}/pull-requests/{number}/diff` show what a pull request changes: the exact commits compared, the changed files with line counts and the patch, cut at file boundaries with a flag when output is too large. `--source-oid` and `--target-oid` read a pair recorded for the pull request even after its branches moved.
- Download a branch, tag or commit as ZIP or tar.gz from the Code tab and commit pages, or with `GET /api/v1/repositories/{id}/archive`. Downloads share the Git transfer limits, and one that fails never leaves a valid-looking archive.
- `owngit repo list`, `show` and `create`, and the matching API routes.
- Inside a clone of an OwnGit repository, `owngit pr`, `owngit check` and `owngit repo` find the server and repository from the `origin` remote.
- `owngit skill --install DIR` installs the coding-tool skill that every build carries, including Homebrew and npm installs.
- `owngit mcp`, a local Model Context Protocol server for coding tools (stdio only). Its tools read repositories, pull requests, diffs and checks, create and review pull requests, and run the checks committed in the working directory; results are the same JSON as the matching commands. The server, repository and credentials are fixed when it starts, and `--no-run-check` removes the tool that runs checks.
- `owngit check task list` lists a repository's check tasks.
- Before setup is finished, the setup link also works from another device by an address OwnGit was not started with. Web setup offers to keep accepting the address you used.

### Changed

- With a shared access password, a correct password is remembered for five minutes, so a clone or push checks it once instead of on every request. On Linux, the extra memory during one clone fell from about 200 MB to about 70 MB. A changed password takes effect at once.
- Clone addresses, runner commands and the recovery command show the configured base URL when one is set.
- The connection status says "Encrypted by Tailscale on this computer" or "Encrypted by the proxy in front of OwnGit" when that is where the encryption happens.
- Terminal setup's "Other devices" step suggests `owngit network set` instead of a one-time `serve --listen`.
- A push or scheduled import that changes no branch or tag no longer makes the next page read every branch and tag again.
- When OwnGit refuses a password or credential file because other accounts can read it, the message says what is wrong (the file mode, or on Windows the owner or the accounts that have access) and prints a command that fixes it.
- The operations guide starts with installing a release instead of building from source, and its examples use `owngit` as installed.

### Fixed

- A clone, fetch, push or archive download whose client stops reading or sending is stopped after 60 seconds without data, instead of holding its transfer slot and the repository for up to 30 minutes. Time that Git spends preparing data does not count, but a client on a link of only a few KB/s can be stopped too.
- Git's keepalive and progress messages reach the client while Git prepares a large pack or runs a slow hook, so a reverse proxy with a read timeout no longer cuts the clone or push as a dead connection. OwnGit starts its answer only after it has read the whole request, which Go-based proxies such as Caddy and Traefik need over HTTP/1.1.
- A push over the request size limit gets HTTP 413 and stops Git at once. Before, it could get a cut response with status 200, and a push with a declared length left Git busy with the repository locked for up to 30 minutes.
- On Windows, a password or token file made by hand in an administrator PowerShell was refused even when only your account could open it. The operations and coding tools guides show PowerShell commands that make such a file.
- `owngit approve-host HOST --state-dir DIR` works as the usage shows.
- A push that OwnGit refused or that was cut off, for example one over the 4 GiB limit, left the data it had received in the repository. OwnGit now removes it when the request ends and logs how much it removed; anything left from earlier is removed at the next start.
- When OwnGit commands opened a new state directory at the same moment, for example the server and a command beside it, one could refuse the state or fail with "database is locked". A crash during the first start could also leave a state directory that refused every later start.
- `approve-host`, `reset-admin`, `setup-link` and `forget-check-container` no longer fail with "state directory changed during inspection" when the running server writes at the same moment.
- An import that ran out of time or was cancelled at certain moments was reported as "state unavailable" or a server error instead of the time limit or the cancellation. A refresh stopped after its branches and tags were already published now completes instead of waiting for the owner to resolve it.
- Restoring a backup no longer lets Git 2.54 or later start its own background maintenance in the repositories being restored. OwnGit turns Git's automatic maintenance off for every Git command it runs.
- Right after startup, a repository that was already prepared could briefly answer as still being prepared.
- On Windows under heavy load, stopping a check or Git process could report a cleanup error when Windows briefly counted no processes in its group.
- When a network failure keeps a runner from reporting a job, the runner log names the job.
- A runner whose token belongs to another repository says so instead of calling the token unknown or revoked.

### Security

- A password or credential file is sent to a server inferred from `origin` only when its first line names that server, and a file that names a server is never sent to another one, so a clone with a hostile `origin` does not receive your secrets.
- Forwarded headers are used only from proxies you name, and `X-Forwarded-Host` is used only when it names a Host OwnGit already accepts.
- Requests forwarded by Tailscale Funnel are refused, and `Tailscale-User-*` headers grant nothing.
- `owngit check run` inspects the worktree without starting the clone's `core.fsmonitor` program or its index hooks.

### Changes for scripts (details)

- New helper credential files written by `helper-credential create` and runner token files written by `runner-credential issue` start with an `owngit-server:` line; scripts that read the file as a bare token must take its last line. Both commands' JSON adds `token_file_server`, and `owngit runner` refuses a token file for another server.
- `owngit pr`, `owngit check` and `owngit repo` no longer fail with `invalid_arguments` when `--server` or `--repository` is missing. They read the clone's `origin` remote and fail with `origin_unavailable`, `origin_ambiguous`, `origin_unsupported` or `origin_server_mismatch` when that is not possible. `owngit check` without `--credential-file` reports "--credential-file is required.".
- A password or credential file whose first line starts with `owngit-server:` must hold exactly `owngit-server: ORIGIN` there, or it is refused with `invalid_credential_origin`. A file without that line must hold the secret on one line; files with more lines are refused (`invalid_password_file`, `invalid_credential_file`).
- `setup-link` and `reset-admin` refuse an argument they do not know instead of ignoring the options after it.
- A gzip Git request body followed by more data, or by a second gzip stream, is refused with HTTP 400 as an invalid body. Before, the extra data was ignored. Git, JGit and libgit2 do not send such bodies.
- `owngit tailscale on` and the Settings switch refuse when HTTPS port 443 already has a Tailscale Serve entry that OwnGit did not make, including one that points at OwnGit. Remove it with the command OwnGit shows, then turn sharing on.

## [1.0.3] - 2026-09-25

**Upgrading:** the state database moves to schema 15 and offline backups to version 10. OwnGit 1.0.3 upgrades a 1.0.x database the first time it starts and still restores backups of versions 1, 2, 9 and 10. Earlier versions refuse the upgraded database and the new backups with a "newer version" message. To go back, restore a backup made before upgrading with the earlier version. Before you upgrade, stop OwnGit and make a backup with your current version's `owngit backup` (see Offline backups in the operations guide). Any 1.0.3 command that opens the state, including `backup`, upgrades the database first, and it logs one line when it does.

**Changes for scripts:** `import add` and `import refresh` exit with status 3 when a finished run left refs that differ from the source, and with status 130 when cancelled. `check run` without `--check` runs only the checks committed in the revision it tests. Commands called without an action, and actions given another action's options, now fail with usage. Details are below.

### Added

- First-run setup in the terminal. When `owngit serve` starts in an interactive terminal on an installation that is not set up, it asks the setup questions there, starting with the language, and hides passwords as you type. You can choose the web dashboard instead: the browser and the terminal show the same short code, and you approve that browser in the terminal. Services, background jobs and redirected output keep the setup link flow. The last step prints a ready-to-copy command for reaching OwnGit over Tailscale when Tailscale is running; it changes nothing.
- Pull requests can be closed without merging and reopened, in the browser, the API and `owngit pr close` and `owngit pr reopen`. Only one open pull request per source and target branch is allowed.
- A sidebar replaces the tab row. Outside a repository it lists Home, All activity, Settings and your repositories by recent activity; inside one it lists the repository's pages. On narrow screens it folds into a menu.
- The repository overview shows the README and the languages the repository is written in. Language colors come from GitHub Linguist.
- Markdown files are rendered, with a Preview and Source switch; folders show their README. Rendering runs in a separate, time- and memory-limited process with raw HTML turned off.
- The Code tab previews PNG, JPEG, GIF and WebP images up to 10 MB and has "Download this file". Long lines do not wrap unless you turn on Wrap lines, which is remembered.
- Commit and pull request pages list the changed files first and show all diffs on one page, with controls to collapse each file or all of them.
- A notice on the dashboard when a new OwnGit release is available, with links to the release notes and install instructions. OwnGit checks GitHub about 30 seconds after it starts and then daily, and sends nothing but its version. Turn it off in Settings or with `owngit serve --no-update-check`. OwnGit does not download or install updates.
- Idle-time repository maintenance. Five minutes after a repository was last changed and used, OwnGit packs its refs and objects and writes a commit graph; between 03:00 and 05:00 it also combines repositories with many packs. It never removes objects, refs or kept history, and it steps aside for pushes, checks and page views.
- Arch Linux and Omarchy: each release includes a `PKGBUILD` that installs the release binary with `makepkg -si`. See the README.
- `owngit <command> --help` prints the command's usage and options.
- Korean versions of the operations, automatic checks, coding tools and security guides.

### Changed

- Pull request changes are compared from the merge base, like `git diff target...source`. Files that only the target branch received are no longer listed. Without a single common commit, the page says so instead of showing a list.
- Repository pages start far fewer Git processes (for example, pull request details 3 instead of 46) and keep recently read folders, files, diffs and comparisons in memory, up to 64 MiB. A page opened again answers even while a push holds the repository. Server memory can peak about 100 MB higher.
- Refs changed directly in the repository folder, without OwnGit, reach code and commit pages after OwnGit next writes to that repository or restarts, as they already did on the dashboard.
- When one repository cannot be prepared at startup, for example because its network share hangs, OwnGit serves the other repositories and keeps retrying that one in the background. Until it is ready, its pages show a notice and Git requests answer 503.
- A busy repository no longer stalls the dashboard. The dashboard waits about a second and then shows the last listing or "In use"; a busy repository page answers 503 with `Retry-After`.
- A missing branch, tag, path or commit answers 404 on every repository tab, with a way back to the repository.
- Check jobs for several branches pushed in quick succession wait for the repository instead of ending as unavailable.
- Saving a new check policy no longer queues again the branches and pull requests that already had a job. Jobs still waiting when a new policy is saved end as `interrupted`.
- `owngit runner` keeps running while OwnGit restarts or the network drops, retrying with backoff. It stops only when its token or requests are refused.
- `owngit import cancel` cancels a first import at any stage before the repository is published and leaves no repository, source or credentials behind.
- On macOS, when Git on the PATH is Apple's `/usr/bin/git` shim, OwnGit runs the Git behind it if the version matches, which halves the cost of each Git process.
- OwnGit holds a lock file in the repository folder while it runs, so a second OwnGit started from a copied state directory stops instead of changing the repositories.
- The Import tab is shown to everyone who can open the repository. The source address, credentials and technical messages are shown only to administrators.
- Password length rules count characters instead of bytes (8 to 1024).
- Light, Dark and System appearance work without JavaScript.
- `import add` refuses a name that already has a repository. `import add` and `import refresh` exit with status 3 when a finished run left refs that differ from the source and list them, and with status 130 when cancelled.
- Commands without an action print usage and exit 1, and actions refuse options that belong to another action.
- When OwnGit upgrades an existing state database, it says so in one line, for example `state database upgraded from schema 14 to 15`: in the server log for `serve`, and on standard error for offline commands such as `backup`.
- A Markdown list or paragraph without blank lines that has more than about 8,000 `*`, `_` or `~` characters (underscores inside words count) shows as source from a smaller size than before, because rendering it can take the whole time limit on a slower machine. Documents made of ordinary paragraphs render as before.

### Fixed

- Clone, fetch and pull failed when Git compressed its request, for example when cloning a repository with 18 or more branches and tags. OwnGit now decompresses these requests with a size limit.
- Parallel Git commands with the correct shared password were refused by the sign-in limiter.
- One repository could take every transfer slot, and a waiting request could wait up to 30 minutes. A repository now uses at most four of the five slots, and a request that waits more than 90 seconds gets 503.
- A stalled or oversized upload held its connection and slot until the 30-minute deadline.
- A repository whose folder was missing or unreadable turned the dashboard into an error page. It is now listed as Preparing or Unreadable.
- Repository names ending with a dot could not be cloned or pushed.
- Merging a pull request whose source the target already contained wrote an empty merge commit. It is now recorded as merged, already up to date.
- A default branch change or repository deletion could fail with 409 while the activity graph was being counted.
- Saving an import token or Basic credential dropped a stored CA certificate, and the reverse. A failed or interrupted first import left its source and credentials behind.
- A ref deleted at the import source was shown as Tracked. It is now shown as Deleted at source.
- After a session expired, submitting a form returns to the page with the form after you sign in again.
- Result notices, such as "Pull request merged", could be shown by opening a crafted address and appeared again on reload. They now appear once, after the action.
- Signing out of shared access was confirmed only after the next sign-in.
- Merging or reviewing after a branch was deleted answered 500 in the API.
- Stopping OwnGit during a Git transfer now ends the transfer and exits normally. Refused pushes are logged.
- On Windows, an automatic check whose command succeeded was sometimes recorded as `error` with a cleanup error such as "owned job membership changed during process capture".
- On Windows, cancelling a Git request, for example when the client disconnects, could leave a Git process that produced no output running until it wrote output or exited on its own. It now stops at once.

### Security

- `owngit check run` without `--check` no longer falls back to a check configuration recorded from another branch. A command that is not committed in the revision being tested never runs on your machine.

## [1.0.2] - 2026-09-24

### Added

- OwnGit can be installed with Homebrew (`brew install juliankang4/tap/owngit`) or npm (`npm install -g owngit`), or downloaded from GitHub Releases. See the README.
- `owngit forget-check-container --job ID --confirm-container-removed` releases the container cleanup record of a finished check job whose Docker daemon OwnGit can no longer reach, for example after Docker was reset. Such a record used to block deleting the repository and cleaning up check workspaces at startup, and nothing could clear it.

### Changed

- Administrator entries are shown to everyone: the repository Settings tab, Delete repository, helper credentials and Automatic checks. Without an administrator session they carry a lock mark, ask for the administrator password when used, and then return to the page you asked for. Administrator data, such as storage paths, stays hidden.
- The storage folder is no longer shown in the top bar. Administrators see it on the Settings page.
- The dashboard and repository pages reuse each repository's branch and tag list until OwnGit next writes to that repository. A dashboard you have opened before no longer starts a Git process per repository, which matters most for repositories on a network share. Refs changed directly in the storage folder without OwnGit appear after OwnGit next writes to that repository or restarts.
- `owngit import credentials` no longer shows the password or token while you type it. Interrupting the prompt with Ctrl+C, `Ctrl+\`, a closed terminal or `kill` (SIGTERM) leaves terminal echo on.

### Fixed

- Choosing the name `new` or `new-import` says that the name is reserved instead of asking for different characters.
- On the Automatic checks page, a number too long for any limit says that it is outside the accepted range instead of "Enter a number".
- During an import, a Git command whose input stopped early because reading the source failed now fails instead of being treated as complete. Smart HTTP push and fetch are unchanged.

### Security

- An import refresh request can no longer make OwnGit read up to 1 MiB of the request before the password is checked. OwnGit now reads at most 4 KiB.
- The page to return to after signing in refuses addresses with control characters. A sign-in link such as `/login?next=/%09/example.com` could otherwise send you to another site after you signed in.

## [1.0.1] - 2026-09-24

### Added

- Repositories have a Settings tab for administrators. It changes the default branch and holds a Delete repository page.
- Administrators can delete a repository. Deletion asks for the exact repository name and the administrator password, and offers two choices:
  - Keep files moves the Git folder to `.owngit-removed/` inside the storage folder. The page then shows a command that pushes its branches and tags into a new repository.
  - Delete files removes the Git folder.
- If OwnGit stops during a deletion, it finishes the deletion at the next start. It does not touch the storage folder while that folder is unavailable.

### Changed

- The repository page shows the activity heatmap first. Below it are a summary of the default branch, branch and tag counts, the last commit, open pull requests and the latest check. Recent commits, branches and tags follow.
- Branch, tag and kept-history lists show the newest entries first and are shortened on repositories with many refs. A Show all link lists every entry.
- The check settings page is renamed Automatic checks. It has a status summary, five numbered setup steps and a copyable example check file. Limits are entered in seconds, minutes, sizes and cores.
- The setup page describes the storage folder accurately: it may be on this computer or on a mounted SMB or NFS share, and settings stay on this computer.
- Pages that cannot read a repository in time show an error page. Requests to the JSON API now stop after 25 seconds instead of 30.

### Fixed

- The dashboard no longer returns an empty reply when repositories are slow to read, for example on a network share with large repositories.
- Startup and the dashboard are much faster with many repositories. Activity counts are cached per branch tip, and the dashboard shows "still counting" instead of waiting.
- The repository sidebar can scroll to its last entry on tall lists.
