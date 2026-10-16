# Storage and backups

This guide is for the person who runs an OwnGit server. It says where OwnGit keeps its data, how to back it up, how to check a backup and how to restore one. Set up scheduled backups soon after the first setup: OwnGit makes no backups until you choose a backup folder.

Kept history protects against force pushes and deleted branches, but it is not a backup.

## Where OwnGit keeps data

| Place | What is in it | Default location |
| --- | --- | --- |
| State directory | `owngit.sqlite` (password hashes, pull requests, checks, imports, settings) and the import credentials in `import-credentials/` | `owngit` in the system's config folder: `~/.config/owngit` on Linux, `~/Library/Application Support/owngit` on macOS, `%AppData%\owngit` on Windows. A Linux system service uses `/var/lib/owngit/state`. |
| Repository folder | One bare Git repository per project, named `NAME.git` | The folder you chose during setup |

Both stay when you uninstall OwnGit. Delete them yourself when you no longer need them.

### State directory

- Keep it on a local disk. OwnGit refuses a state directory on a network share, a FUSE or 9P file system, or a virtual machine's shared folder. This includes a Windows drive seen from WSL, such as `/mnt/c`, and a Docker Desktop bind mount. Use a folder in WSL's own file system or a Docker named volume instead.
- Protect it like a password. Import credentials are stored there without encryption, readable only by the account that runs OwnGit.
- On macOS and Linux, OwnGit refuses a state directory that another account could replace. Run the `chmod` command it prints, or choose another place.

### Repository folder

- It can be on another disk or on a mounted SMB or NFS share.
- Only one OwnGit server may use it at a time. A running server holds `.owngit-serve.lock` in the folder, and a second server on the same folder refuses to start.
- If the folder is empty when OwnGit starts, such as the mount point of a share that is not mounted yet, OwnGit takes that lock only when it first writes a repository there.
- If the folder or its `.owngit-serve.lock` is replaced while OwnGit runs, or the lock file is removed, for example when a share is mounted, unmounted or mounted again over it, OwnGit stops writing there. Pushes, pull request changes, imports, restores, maintenance, and creating, deleting or renaming a repository or changing its default branch are refused with "The repository folder changed after OwnGit started" (HTTP 409 for Git and the API). Imports are recorded as [unresolved](REPOSITORIES.md#unresolved-publications). Browsing and clones keep working. If the original folder and its `.owngit-serve.lock` come back, writes continue without a restart. If the lock file was removed, or to use the folder now in place, restart OwnGit. It then uses the folder it finds at start.
- OwnGit also remembers each repository's own folder (`NAME.git`) when OwnGit starts or when it creates the repository. Before each write and each backup, it checks that this is still the same folder. If a different folder is in its place, OwnGit refuses the write with the same message, and a backup fails without saving a partial result. To continue, put the original folder back. To use the folder now in place, restart OwnGit. It then uses the folder it finds at start. OwnGit never switches to another folder while it runs.
- If the folder or a repository's own folder is missing or cannot be read, OwnGit reports the storage as unavailable. When the original folder or disk comes back, writes work again without a restart. Renaming a repository and changing its import settings need only the repository folder, not the repository's own folder.
- OwnGit leaves files it did not create alone. It may create these folders there:

| Folder | What it is |
| --- | --- |
| `.owngit-removed/` | Repositories removed from OwnGit with their files kept. They are not in backups. |
| `.owngit-failed-create/` | Empty folders from a repository creation that failed. They are not repositories. Check them, then delete them. |

## Repository maintenance

After a repository changes and then goes unused for 5 minutes, OwnGit packs its refs and objects. In a daily window, 03:00 to 05:00 local time, it also combines the packs of any repository with more than 20 packs. Maintenance never deletes a ref or kept history.

Change it under Settings, Storage & recovery, Repository maintenance, or with `owngit settings set`:

| Option | Default | Range |
| --- | --- | --- |
| `--maintenance` | `on` | `on` or `off` |
| `--maintenance-window` | `3-5` | Whole local hours, such as `22-6` |
| `--maintenance-idle` | `5m` | `1m` to `24h` |
| `--maintenance-step-time` | `30m` | `1m` to `24h` |
| `--maintenance-consolidation-time` | `2h` | `1m` to `24h` |
| `--maintenance-packs` | `20` | `2` to `1000` |

### Unused object cleanup

Unused object cleanup deletes Git objects that no ref reaches, such as commits a force push left behind when kept history was off. It is off by default. Turn it on under Settings, Storage & recovery, Unused object cleanup, or with:

```sh
owngit settings set --unused-object-cleanup on --cleanup-grace-days 14
```

- It deletes an object only after the grace period: 14 days by default, 2 to 365 days allowed. Deleted objects are gone for good.
- It never deletes anything that a branch, tag, kept history or pull request still reaches.
- It runs at night, in the maintenance window, and only while maintenance is on.
- It needs Git 2.37 or newer on the server.

Cleanup does not remove a pushed secret that kept history still reaches. See [Pushed secrets](#pushed-secrets).

## Scheduled backups

Choose a backup folder and turn scheduled backups on. In the dashboard, open Settings, Storage & recovery, Backups. On the command line:

```sh
owngit backup schedule set \
  --destination /absolute/path/to/backups \
  --server http://127.0.0.1:7654 --accept-insecure-http \
  --password-file /path/to/admin-password.txt
```

The first backup starts as soon as you save.

The commands `owngit backup schedule`, `now`, `status`, `runs`, `check`, `download` and `upload` talk to the running server. Each needs `--server` and a `--password-file` with the administrator password ([Operations](OPERATIONS.md) explains password files). A plain `http://` address also needs `--accept-insecure-http`. The examples below leave these flags out.

| Option | Default | Choices |
| --- | --- | --- |
| `--destination` | none; required the first time | An absolute path on the server computer, outside the state directory and the repository folder. OwnGit creates it, private to its account, when it is missing. |
| `--interval` | `1d` | `12h`, `1d` or `7d` |
| `--keep` | `7` | `1` to `1000` |
| `--verify` | `on` | `on` or `off` |

- `owngit backup schedule show` prints the schedule. `owngit backup schedule off` stops scheduled backups. Existing backups stay.
- A scheduled backup runs only while OwnGit runs. If OwnGit was stopped when one was due, one backup starts when OwnGit starts again.
- A failed scheduled backup is not retried before the next interval. Run `owngit backup now` to try again.
- With `--verify on`, OwnGit rehearses a restore of each new backup in the system's temporary folder. That disk needs room for the repositories. A verification that takes longer than 2 hours fails.

### Backup plans

A backup **plan** is a named set of a target directory, an interval, a retention count, and whether to verify. OwnGit keeps up to 50 plans, so the same OwnGit can back up to both the local disk and a folder on an offline or network-mounted drive. Every backup and its recorded run are named with the plan that started it.

A plan name uses letters, digits, underscores and dashes, up to 64 characters (`offsite`, `disk2`, `7d-local`). The plan the first set creates, and every command below without `--plan`, is named `default`.

```sh
# Create a plan. The defaults are scheduled on, daily, keep 7, verify on;
# each flag overrides one.
owngit backup plan create --name offsite --destination /mnt/offline/backups \
  --interval 7d --keep 3 --verify on

# List plans and read one plan's state.
owngit backup plan list
owngit backup schedule show --plan offsite

# Change or switch off one plan, back it up by hand, and read its runs.
owngit backup schedule set --plan offsite --keep 2
owngit backup schedule off  --plan offsite
owngit backup now    --plan offsite
owngit backup status --plan offsite
owngit backup runs   --plan offsite

# Delete a plan that is not making a backup.
owngit backup plan delete --name offsite
```

In the dashboard, the Backups group lists the saved plans in a chooser: select one to view or change its folder, interval, retention and verification, to back it up now, and to read its recorded backups. A "New plan" form creates a plan, and each plan other than `default` has a "Delete plan" button.

Deleting a plan refuses while one of its backups is running. Deleting removes only the plan's configuration: the backups it wrote and their run history stay listed under the plan's name and stay on disk. OwnGit removes nothing for a deleted plan, so those backups no longer have automatic cleanup; delete their folders by hand when they are no longer needed.

Scheduled backups of all plans share one slot: only one backup, verification or upload runs at a time. When several plans are due, the one due first starts, and equal due times keep the plans' stable order. When a target folder is unavailable, only that plan's run fails; the other plans still run. After fixing the target, retry its plan by hand with `owngit backup now --plan NAME`.

### Back up now

Choose "Back up now" on the Storage & recovery tab, or run:

```sh
owngit backup now
```

The backup goes into the plan's folder, even while that plan's scheduled backups are off. Add `--plan NAME` to back up another plan. The command returns at once. Only one backup, verification or upload runs at a time.

### Check backups

| Command | Shows |
| --- | --- |
| `owngit backup status` | The default plan's schedule, running backup, last backup, newest verified backup and next due time; add `--plan NAME` for another plan |
| `owngit backup runs` | Every recorded backup with its `plan`, `id`, `status`, `verification`, `path` and `message`; add `--plan NAME` to list one plan |
| `owngit backup check --run ID` | Starts a new verification of a kept backup. `ID` is the 32-character `id` from `owngit backup runs`. |

A run's `status` is `running`, `succeeded`, `failed` or `interrupted`. Its `verification` is `passed`, `failed` or `not_run`. Only `passed` means the backup was verified. The dashboard shows the same under Current state and Recorded backups.

### Which backups OwnGit keeps

Each backup is a new folder in the plan's destination, named `owngit-backup-YYYYMMDD-HHMMSS-XXXXXXXX` (the start time in UTC). After a successful backup, OwnGit keeps the newest `--keep` backups of the plan plus the plan's newest verified one, and removes older backups of that plan. Cleanup is per plan, even when two plans use the same folder: one plan never deletes another plan's backups.

- OwnGit deletes only backups it made and recorded. Other files in the folder stay. A backup folder that you changed, for example by adding a file, also stays, and the next backup's message names it.
- A backup that fails its verification does not count toward `--keep` and never replaces a verified backup. OwnGit keeps only the newest failed one, so you can inspect it, and deletes its older failed ones. The next backup that passes verification deletes it too. A verification that cannot finish, for example because it ran out of time or temporary space, counts as failed.
- When OwnGit cannot delete an older failed backup, the failed run's message says so after the reason for the failure. The next backup tries again.
- Nothing is deleted after a backup that failed for another reason or was interrupted.
- The folder needs room for one more backup, because the new one is written before an old one is deleted. When the free space is less than the last backup's size, the backup fails at once with `not enough free space in DIR`.

## Download a backup

Choose Download on a backup under Recorded backups, or run:

```sh
owngit backup download --run ID --output /path/to/new-file.tar
```

The `.tar` file holds every repository, OwnGit's records and the password hashes. Keep it as private as the backup folder. To check it on another computer, unpack it with `tar -xf FILE` and run `owngit backup verify` on the folder it makes.

## Back up a stopped OwnGit

When OwnGit is not running, back it up directly. The output folder must not exist:

```sh
owngit backup --state-dir /path/to/owngit-state --output /path/to/new-backup
```

This command refuses with `offline_required` while an OwnGit runs with that state directory. Use `owngit backup now` instead.

## What a backup holds

A backup is a folder with a `manifest.json` and one Git bundle per repository. It holds:

- every branch, tag and other ref, kept history, and each repository's HEAD and own settings;
- pull requests, reviews, tasks, checks, check policies and import sources;
- the access mode and the password hashes.

A backup holds only the commits that a ref or HEAD reaches. With kept history off, a commit overwritten by a force push is not in later backups.

A backup does not hold sign-ins, network settings, helper credentials, runner tokens, import credentials and schedules, share links, consent to run checks, raw check logs, the backup schedule and history, or the server-wide settings. [After a restore](#after-a-restore) says how to set them up again.

An alias branch (a branch that points to another branch, a Git symbolic ref) is saved as an ordinary branch. The backup's message lists each alias with the `git symbolic-ref` command that reconnects it after a restore. Keep that message: the backup itself does not record the targets.

A backup is refused for a repository that borrows objects from another repository (`objects/info/alternates`) or is a partial clone. The message names the repository.

### Pushed secrets

A pushed secret stays in the repository's Git data even after a force push or branch deletion. While a ref or kept history still reaches it, every later backup holds it too. Deleting the repository with its files removes it from OwnGit, but earlier backups still hold it. Rotate any secret you push by mistake.

## Verify a backup

Check that a backup restores, without changing it or the running server:

```sh
owngit backup verify /path/to/backup
```

The command rehearses a full restore in a private folder in the system's temporary folder, then deletes that folder. It checks each bundle's SHA-256 hash, the refs, `git fsck`, and the database. It needs no server password, only read access to the backup.

- Exit status 0 means verified, 1 means not verified, 130 means you stopped it with Ctrl+C.
- The temporary folder's disk needs room for the repositories. Use `--temp-dir DIR` to rehearse elsewhere.
- `--json` prints the result as JSON.

The hashes detect damage. They cannot detect a backup that someone replaced together with its manifest, so keep backups where others cannot write.

## Restore a backup

A restore never writes into an existing folder. It creates a new state directory and a new repository folder:

```sh
owngit restore \
  --input /path/to/backup \
  --state-dir /path/to/new-owngit-state \
  --repository-root /path/to/new-repositories \
  --verify
```

`--verify` rehearses the restore first and restores only a backup that passes. Restore also checks every bundle, ref and record before it finishes, and names any repository that fails.

Restore preserves malformed Git history and reports each affected repository in text and JSON (`object_warnings` entries with `id` and `message`, omitted when there are no warnings), with instructions to run `git fsck --strict` in a separate copy, repair the history, and import the repaired repository.

- The disk of the new repository folder needs room for all bundles plus the largest one again. Restore checks this first.
- Ctrl+C stops a restore. It removes what it made and exits with status 130.
- OwnGit restores backups made by the same or an earlier version. An earlier version may refuse a backup from a later one.

### Replace the installation you use

"Restore this backup", on a backup under Recorded backups, shows these steps with your folders filled in:

1. Stop OwnGit: `owngit service stop`, or end `owngit serve` with Ctrl+C.
2. Rename the current state directory and repository folder by adding `.before-restore` to their names.
3. Run the `owngit restore ... --verify` command shown, with the original folder names as targets. On a Linux root install, where OwnGit runs as the `owngit` account, it starts with `runuser -u owngit --`: run it as root. On Windows, run it in PowerShell.
4. Behind a reverse proxy, save the public address and the proxy again with `owngit network set --state-dir <state folder> --base-url <public address> --trusted-proxy <proxy address>`. The restore steps show this command with the state folder filled in, ready to copy. `--state-dir` saves the settings in the restored state folder. A restore resets network settings, and saved settings apply at the next start.
5. Start OwnGit again: `owngit service start`, or the way you started it before.

Copy the commands from the page instead of typing `owngit`. When the `owngit` on PATH is not the program that runs OwnGit, for example after an archive or source install, the page names the program by its full path.

The `.before-restore` folders stay until you delete them. Keep them until you have checked the restored server.

### Restore from a downloaded file

Upload the `.tar` file under "Restore from a backup file" on the Storage & recovery tab, or run:

```sh
owngit backup upload --input /path/to/backup.tar
```

OwnGit verifies the upload. When it passes, the page shows the restore steps above, and `owngit backup status` shows the command as `upload_restore_command`. Nothing changes until you run it.

- OwnGit keeps one upload, in `backup-uploads` inside the state directory. It deletes it after 24 hours, when OwnGit starts, and when verification fails. So run the restore before you start OwnGit again.
- The state directory's disk needs free space for the file plus 1 GiB.
- When a browser shows a connection error instead of a reason, try `owngit backup upload`, which prints the reason.

### After a restore

Start OwnGit with the restored folders before you use them in any other way. Then set up again what a backup does not carry. `owngit restore` prints each item with where to set it:

- Everyone signs in again.
- Network settings, Tailscale sharing and share links.
- Helper credentials and runner tokens: the old ones are refused, so create new ones.
- Consent to run automatic checks.
- Import credentials, each source's connection choices, and import schedules.
- The backup plans. Earlier backups are no longer listed, but their folders stay and `owngit restore` still reads them. After moving OwnGit to another computer, recreate your plans (`owngit backup plan create`, or the New plan form); the old plans are safe to leave on the computer they backed up.
- Backup before an upgrade is turned on again. Turn it off again if you had turned it off.
- Server-wide settings are back at their defaults. Set them again under Settings or with `owngit settings set`.

Behind a reverse proxy, OwnGit refuses pages and Git through the proxy until its network settings are saved again. The answer is 421 (`unrecognized host`) or 403 (`origin does not match this server`). Save the public address and the proxy with `owngit network set --state-dir <state folder> --base-url <public address> --trusted-proxy <proxy address>`, then restart OwnGit. Give `--state-dir` when OwnGit uses a state folder other than its default.

### If a backup or restore stops

A restore that stopped without cleaning up, for example after a power loss:

1. Do not start OwnGit from either target. Do not remove a `.owngit-restore-pending` file.
2. Move both targets and any `TARGET.owngit-restore-...` folders beside them to another place.
3. Restore again into new folders.

A backup that stopped is recorded as `interrupted` and is never a finished backup, whatever it left on disk. A hidden `.NAME.owngit-backup-...` folder beside it holds the unfinished part. A complete-looking folder from an interrupted run is not verified: run `owngit backup verify` on it before you rely on it.

## Where backups can be written

- On macOS and Linux, no other account may be able to change the backup folder or any folder above it. A sticky folder such as `/tmp` is accepted. Otherwise OwnGit names the folder and prints the `chmod` command that fixes it.
- Backups may go to a network share, unless OwnGit runs as root or as a Windows administrator.
- Backups work on exFAT, FAT and NFS. Restoring repositories onto those file systems does not: `owngit restore` stops, names the file system and changes nothing. Restore the repositories to another disk. The dashboard warns under Current state when the backup folder is on such a disk.
- The restored state directory must be on a local disk, as every state directory must.

## Backup before an upgrade

When a newer OwnGit starts on an older state, it first backs up the state and all repositories, then upgrades. It upgrades only after that backup is complete.

- The backup goes into a folder beside the state directory named after it with `-backups`, for example `~/.config/owngit-backups`. That disk needs room for every repository.
- The server log says where the backup is and prints the command that restores it. The file `owngit-upgrade-backup.txt` in the backup says the same.
- When the backup fails, for example on a full disk, OwnGit does not upgrade and stops with the reason. The earlier version can still use the state. Fix the cause and start again.
- An earlier OwnGit refuses a state that a newer one upgraded. To go back, stop OwnGit, move the state directory aside, and run the printed restore command with the earlier version.

To upgrade without this backup, for example when you back up another way:

```sh
owngit upgrade-backup off
```

`owngit upgrade-backup on` turns it back on, and `owngit upgrade-backup` shows the setting. With it off, make a backup yourself before you install a newer version.

## Who can see backups

Only the administrator can manage backups. The dashboard's backup sections show only to a browser confirmed as administrator, and the `owngit backup` commands that talk to the server need the administrator password. Coding tools get a summary through the `backup_status` MCP tool, which names no folder, repository or error. `owngit backup --output`, `owngit backup verify` and `owngit restore` need no server password, only access to the files on this computer.
