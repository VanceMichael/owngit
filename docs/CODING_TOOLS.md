# Coding tools: connect Codex, Claude Code and other agents to OwnGit

<p align="center"><b>English</b> | <a href="CODING_TOOLS.ko.md">한국어</a></p>

This guide shows how to let a coding tool such as Codex, Claude Code or Pi
work with an OwnGit server. The tool can then list repositories, open,
review and merge pull requests, and record the results of project checks.

A tool uses OwnGit in one of two ways:

- It runs the `owngit` command and reads the JSON it prints. The
  [owngit-checks skill](../integrations/skills/owngit-checks/SKILL.md) tells
  the tool which commands to run.
- A tool that supports MCP (Model Context Protocol) starts
  [`owngit mcp`](#mcp-server) and calls the same commands as tools.

The commands print JSON. A failure has a stable `error.code` and a message
that says what to do.

## Quick start

1. Open **Coding tools** in the dashboard sidebar (`/coding-tools`). It shows
   this server's address and the commands below, ready to copy.
2. Install the skill where your coding tool looks for skills:

   ```sh
   owngit skill --install ~/.agents/skills
   ```

3. To use MCP, add the server to your tool:

   ```sh
   claude mcp add --scope user owngit -- owngit mcp --server https://owngit.example.test
   codex mcp add owngit -- owngit mcp --server https://owngit.example.test
   ```

   If the server asks for a password, the page adds `--password-file`.
   For the check tools, also add `--credential-file`.

4. To record checks, [create a helper credential](#helper-credentials).

Inside a clone of an OwnGit repository you can leave out `--server` and
`--repository`; see [Inside a clone](#inside-a-clone).

## Get the owngit command

Homebrew, npm and the Arch Linux package put `owngit` on `PATH`. The
one-line installer, a portable archive and a source build do not change
`PATH`. In that case, give the coding tool the full path of the program
instead of editing shell startup files for it.

## Install the skill

`owngit skill --install DIR` writes `DIR/owngit-checks/SKILL.md`. Each
`owngit` binary carries the skill that shipped with it, and the portable
archives also carry it at `integrations/skills/owngit-checks/SKILL.md`.

If the installed file has your own edits, the command stops with
`skill_modified` and changes nothing. Compare it with `owngit skill --print`,
then add `--replace`. The old file is kept beside the new one.

Where tools look for skills:

| Tool | Project | User |
| --- | --- | --- |
| Codex | `.agents/skills/` | `~/.agents/skills/` |
| Pi | `.agents/skills/` or `.pi/skills/` | `~/.agents/skills/` or `~/.pi/agent/skills/` |

Call the skill by name, because a tool can miss a match by description. In
the Codex app select it with `@`, in the Codex CLI mention it with `$`, and
in Pi use `/skill:owngit-checks`.

## Access and credentials

OwnGit has three kinds of secret. The shared password is the one every user
of the server signs in with (general access). Each secret lives in a file
that only your account can read. Never put a secret in a command argument, a document or a log.

| Secret | Used by | Flag |
| --- | --- | --- |
| Shared general-access password | `owngit pr`, `owngit repo list`, `show`, `create`, `owngit tasks`, most MCP tools | `--password-file` (leave it out when access is open) |
| Helper credential | `owngit check` and the MCP check tools, for one repository | `--credential-file` |
| Administrator password | `helper-credential`, `check-policy`, `check-job`, administrator `repo` commands | `--password-file` |

Plain HTTP is refused unless you add `--accept-insecure-http` to that
command. Add it only when you accept that the connection is unencrypted.
A coding tool must not add it on your behalf.

### Helper credentials

A helper credential lets a tool record checks for one repository. Create it
with the administrator password:

```sh
owngit helper-credential create \
  --server https://owngit.example.test \
  --repository example-project \
  --label laptop \
  --password-file /path/to/admin-password-file \
  --output /path/to/helper-token
```

- The token is written only to the new `--output` file. The server keeps only
  a hash, so a lost token cannot be shown again; create a new one.
- An existing file at `--output` is never replaced.
- `owngit helper-credential list` and `revoke --id ID` manage credentials.
  The repository's **Helper credentials** page, linked from its Checks tab,
  does the same. A revoked token stops working at once.
- The Coding tools page lists every repository's credentials to a browser
  confirmed as administrator. A label is the name given at creation, not
  proof of who used it.

### Credential files and the server line

A password or token file can start with a line that names the server it
belongs to:

```text
owngit-server: https://owngit.example.test
SECRET
```

Such a file is sent only to that server. `helper-credential create` and
`runner-credential issue` write this line for you. A file without the line
works only with an explicit `--server`.

When OwnGit reads the server from a clone's `origin` remote, the file must
have this line. A clone's `origin` can point anywhere, so the line is what
tells OwnGit that you trust that server.

To add the line to a password file on macOS or Linux, write a new private
file:

```sh
(umask 077; { printf 'owngit-server: %s\n' https://owngit.example.test; cat password-file; } > bound-password-file)
```

On Windows, a file made with Notepad or `echo` can be read by others and is
refused. In PowerShell (5.1 or 7), create a file only your account can read,
type the password at the prompt, and add the server line:

```powershell
$file = "$HOME\owngit-password.txt"
$f = New-Item -ItemType File -Path $file
$io = if ($PSVersionTable.PSEdition -eq 'Core') { [IO.FileSystemAclExtensions] } else { [IO.File] }
$acl = $io::GetAccessControl($f, 'Access')
$acl.SetSecurityDescriptorSddlForm("D:P(A;;FA;;;$([Security.Principal.WindowsIdentity]::GetCurrent().User))", 'Access')
$io::SetAccessControl($f, $acl)
[IO.File]::WriteAllText($file, [Net.NetworkCredential]::new('', (Read-Host -AsSecureString 'Password')).Password)
[IO.File]::WriteAllText($file, "owngit-server: https://owngit.example.test`n" + [IO.File]::ReadAllText($file))
```

A script that reads a token file itself must use its last line.

## Inside a clone

In a clone of an OwnGit repository, `owngit pr`, `owngit check` and
`owngit repo` read a missing `--server` and `--repository` from the
`origin` remote. `origin` must be an OwnGit clone address,
`http(s)://HOST[:PORT]/git/NAME.git`. Flags you pass always win. The command
prints one line on standard error that says what it read.

`check run` reads the clone that holds `--workdir`. The other commands read
the clone that holds the current directory.

After a repository is renamed, `owngit pr` and `owngit repo` stop at the old
address with `repository_moved`, and `details.address` gives the new name.
Check commands keep working at the old address for 90 days. Update the clone:

```sh
git remote set-url origin https://owngit.example.test/git/new-name.git
```

## Repositories

`owngit repo list`, `show` and `create` use general access:

```sh
owngit repo list --server https://owngit.example.test
owngit repo show --server https://owngit.example.test --repository example-project
owngit repo create --server https://owngit.example.test --name example-project \
  --description "Optional description"
```

Administrator commands (rename, delete, settings, default branch, share links) and
restoring files from earlier commits are described in
[Repositories](REPOSITORIES.md).

## Pull requests

A push does not open a pull request. After you push a branch, create one:

```sh
owngit pr create \
  --server https://owngit.example.test \
  --repository example-project \
  --source feature-branch \
  --target main \
  --title "Describe the change" \
  --body-file description.md \
  --review request
```

`--body-file -` reads the Markdown description from standard input.
`--review skip` records that review was skipped on purpose; it is not an
approval.

Then work with it by number. Review and merge commands take the exact source
and target commits that `pr show` printed:

```sh
owngit pr list --state open
owngit pr show --number 1
owngit pr diff --number 1 --stat
owngit pr mergeability --number 1
owngit pr edit --number 1 --edit-revision 0 --title "New title"
owngit pr review request --number 1 --source-oid SOURCE_OID --target-oid TARGET_OID
owngit pr review submit --number 1 --source-oid SOURCE_OID --target-oid TARGET_OID \
  --decision approved --reviewer "reviewer label" --note-file note.md
owngit pr review skip --number 1 --source-oid SOURCE_OID --target-oid TARGET_OID
owngit pr merge --number 1 --source-oid SOURCE_OID --target-oid TARGET_OID
owngit pr close --number 1
owngit pr reopen --number 1
```

What to know:

- Only one pull request can be open for a source and target pair. A second is
  refused with `pull_request_exists`.
- If a branch moved after you read it, a review or merge fails with
  `stale_revision`. Read the pull request again and decide based on the new commits.
- `pr edit` needs the `edit_revision` from `pr show`. If someone edited the
  pull request in between, it fails with `stale_edit`; show it again and
  reapply your change.
- `pr diff` shows the changes from the merge base to the source. `--stat`
  leaves out the patch and `--patch` prints only the patch. Large diffs are
  cut at file boundaries and the result says so (`truncated`).
- A file too large for the server to compare as text has `too_large: true`,
  no `additions` or `deletions`, and no section in the patch. When such files
  are all that the patch leaves out, `reason` is `too_large`, and
  `pr diff --patch` names them on a note line. The changes in
  `repo restore preview` mark such files the same way. A file whose line
  counts were not read, as when a change holds more than 100 too-large files,
  also has no `additions` or `deletions`.
- `pr mergeability` answers `clean`, `conflict`, `unavailable` or `stale`. It
  changes nothing, and `pr merge` checks again when it runs.
- A merge is a fast-forward or a merge commit by `OwnGit <owngit@localhost>`.
  It never squashes, rebases or deletes the source branch, and a retried merge
  never makes a second commit. Merging needs Git 2.38 or newer on the server.
- Reviews and checks are advisory. Neither blocks a merge.
- `pr list` shows 50 pull requests at a time, newest first. When more remain,
  the result has `next`; pass it as `--before` for the next page.

### Automatic merge

Instead of watching the page and merging by hand after checks finish, enable
automatic merging on an open pull request. OwnGit then merges it itself once
the current source commit's checks pass.

```sh
owngit pr auto-merge --number 1
owngit pr auto-merge --number 1 --cancel
```

You can also enable or cancel it from the pull request page, or with the owner
API:

```sh
curl -X POST -u owngit:PASSWORD \
  https://owngit.example.test/api/v1/repositories/example-project/pull-requests/1/auto-merge
curl -X POST -u owngit:PASSWORD \
  https://owngit.example.test/api/v1/repositories/example-project/pull-requests/1/auto-merge/cancel
```

What to know:

- The intent belongs to the pull request, not to a commit: it survives a push
  to the source branch. The merge is always decided for the **current** source
  and target commits.
- OwnGit merges only when the latest check evidence of the current source
  commit finished with a pass, really tested that commit from a clean worktree,
  used the check configuration committed at that commit, and was readable.
  Missing evidence, checks still running, a failed verdict, a dirty worktree, a
  changed configuration, or an unreadable record shows a clear waiting or
  blocked state on the pull request and publishes no merge.
- With no check evidence at all for the current commit, the request keeps
  waiting, even when no checks are configured. It merges once a helper or
  configured check records a passing result for that commit.
- When the source branch moves, an older pass is never reused; the request
  waits for checks of the new commit. When the target branch moves, OwnGit
  re-checks the current pair and its mergeability, and a conflict only waits
  until a later ref change resolves it.
- You can cancel at any time. Closing a pull request cancels the intent too;
  reopening does not bring it back. If a merge was already published while a
  cancel or close was queued, the pull request is reported as merged and is not
  merged twice.
- Pending intents survive a restart and backup restore. When the repository is
  temporarily unavailable, OwnGit keeps the intent and retries on the next
  check completion or ref change.
- Without automatic merging, nothing changes: checks and reviews stay
  advisory and manual `pr merge` works exactly as before, including while
  checks fail or are still running.

## Recording checks

`owngit check` (the helper) runs a project's checks on your own computer
and records the result on the server, tied to the exact commit it tested. It needs a
[helper credential](#helper-credentials). For checks that OwnGit runs by
itself on each push, see [Automatic checks](AUTOMATIC_CHECKS.md).

The checks run with your account's permissions and can reach your files. They
are not sandboxed. They get only a few variables from your shell's environment,
such as `PATH`, `HOME` and the locale settings. They do not get proxy settings,
tool settings such as `JAVA_HOME`, or credentials. `TMPDIR`, `TEMP` and `TMP`
point to a new private folder in the system's temporary folder, which is
removed after the run. If a command needs another variable, set it in the
command, for example `HTTPS_PROXY=http://proxy.example.test:3128 go test ./...`.
When a check command fails or cannot start, its log begins with the names of
the variables it did not get. A temporary-folder error is reported as its own
error, without that note.
[Environment variables](AUTOMATIC_CHECKS.md#environment-variables) has the
full list.

1. Create one task for the unit of work. Keep its ID while the commits change.

   ```sh
   owngit check task new \
     --server https://owngit.example.test \
     --repository example-project \
     --credential-file /path/to/helper-token \
     --title "Fix the failing build"
   ```

2. Run the checks. Without `--check`, the command runs the checks in the
   `.owngit/checks.json` committed in `HEAD` of `--workdir` (default `.`).
   `--check name=command` runs exactly the checks you give instead.

   ```sh
   owngit check run --task TASK_ID \
     --server https://owngit.example.test \
     --repository example-project \
     --credential-file /path/to/helper-token
   ```

3. Before an agent tries a fix, reserve a correction round, and pass it to the
   run that verifies the fix:

   ```sh
   owngit check cycle reserve --task TASK_ID ...
   owngit check run --task TASK_ID --cycle CYCLE_ID ...
   ```

4. Read what is recorded:

   ```sh
   owngit check task list ...
   owngit check status --task TASK_ID ...
   owngit check log --attempt ATTEMPT_ID ...
   ```

`...` stands for the same `--server`, `--repository` and `--credential-file`
flags. Each check gets 10 minutes and 64 KiB of output by default
(`--timeout`, `--output-limit`). `--no-upload` runs the checks locally and
records nothing. For how long the server keeps raw logs, see
[Raw check logs](AUTOMATIC_CHECKS.md#raw-check-logs).

Git gets 30 seconds to read the committed `.owngit/checks.json`. In a partial
clone (a clone made with `--filter`), the file may still be on the remote. If
the read does not finish in time, the run stops with `revision_unavailable`
and records nothing. To download the file, run
`git show REVISION:.owngit/checks.json` once, where `REVISION` is the commit
the message names. Then run the check again.

### Reading the result

`check run` exits with:

| Exit | Meaning |
| --- | --- |
| `0` | Every check passed. |
| `1` | A check did not pass, or the run stopped before any check ran (for example `checks_not_configured`). |
| `2` | The checks ran, but this client could not confirm that the server recorded them. `upload_error` says why. |
| `128` plus the signal number | A signal stopped the run: `130` for Ctrl-C, `129` for a closed terminal, `143` for `SIGTERM`. |

A signal stops the checks, but once registration has started the client still
lets registration and completion finish, so the attempt is recorded. The exit
status names the signal, even when the checks had already
passed. It is `2` instead when the client could not confirm the record, and
`1` when the checks had finished without passing before the signal arrived or
a check ended in `error`. A few cases differ:

- A signal during preparation, before registration starts, stops the run
  there. Nothing runs, nothing is recorded and no result is printed.
- A second signal ends the wait for registration or completion at once, and
  the exit status names that signal. The server may still have recorded the
  attempt, so run `check status --task TASK_ID` to see what it kept.
- A run started with `SIGHUP` ignored, as `nohup` starts one, is not stopped
  by a closed terminal and exits with the status of its checks.

Each check ends as `passed`, `failed`, `error`, `cancelled`, `incomplete`
(it exceeded its time or output limit) or `unavailable`.

`attempt.worktree_state` says whether the clone stayed clean. The client reads
it with Git before and after the checks, and each reading has 30 seconds. The
state is `dirty` when a check changed tracked files or moved the commit. It is
`unknown` when a reading failed, did not finish in 30 seconds, or was stopped
by a signal, so a run stopped during its checks records `unknown` unless a
change was already seen. When the state is `unknown`, `worktree_note` in the
result says why, in the same words as the first line of the recorded log. The
field is absent when the state was read. Do not call a `dirty` or `unknown`
commit tested.

Thirty seconds leaves wide room for a typical checkout whose files are already
cached. A very large checkout with a cold file cache, above all on Windows, can
still exceed it and record `unknown`.

### Processes a check starts

When a check ends, the client stops the programs the check started. This
happens when the check finishes, runs out of time or is stopped. The Git reads
the client runs itself are handled the same way. Start long-lived programs,
such as a database or a development server, outside OwnGit.

How much is stopped depends on the system:

- Linux: every process the check started, including one that started a new
  session (for example with `setsid`).
- macOS and other Unix systems: the check's process group only. A process
  that started a new session keeps running.
- Windows: every process the check started, because a job object holds them.

On Linux the client puts a mark in the check's environment and reads it back
from `/proc` to find a process that left the process group. It cannot find a
process that cleared its environment or runs as another account. It also
cannot find a process marked not dumpable, because the kernel hides that
process's environment. If the system has no `/proc`, the client stops only the
process group and logs one line.

If OwnGit cannot end every process a check started within its cleanup time
(about 5 seconds on Linux), or cannot read `/proc` to find them, the check ends
as `error`. Its `cleanup_error` field says which of the two happened.

### Correction rounds

A task has three correction rounds. Reserve one before each automatic fix.
The first run and manual reruns use none. When none are left,
`check cycle reserve` fails with `correction_budget_exhausted`: stop and report
the open task. A manual run can still be recorded.

Read the remaining rounds only from `check status` or from the `task` object
in a recorded run. The top-level `correction_cycles_remaining` prints `0` when
the client did not hear from the server, which happens in two cases:

- `--no-upload` never contacts the server.
- A registration that failed (`registered` is false and `upload_error` is set)
  is unconfirmed. The server may or may not have recorded the attempt.

For an unconfirmed attempt, keep the task ID, save the printed `attempt_id`
as diagnostic evidence, and run `check status --task TASK_ID`. Do not rerun
the check or reserve a round to find out: a rerun is always a new attempt. If
`check status` does not show the attempt, report it as unconfirmed. Never
treat that `0` as an exhausted budget.

## MCP server

`owngit mcp` is a local MCP server. It talks over standard input and output
(the stdio transport) and opens no network port. Each tool runs one `owngit`
command and returns its JSON. A tool that can run shell commands can use the
command line instead, which usually costs fewer tokens.

### Start options

The coding tool starts the server. Its flags set what the tools may reach:

| Flag | Meaning |
| --- | --- |
| `--workdir DIR` | The clone whose `origin` names the server and repository, and where `check_run` runs. Use an absolute path. |
| `--server`, `--repository` | Override `origin`. Without a repository, tools take a `repository` argument. |
| `--password-file` | Shared general-access password. |
| `--credential-file` | Helper credential. Adds the check tools. |
| `--accept-insecure-http` | Allow a plain HTTP server. |
| `--no-run-check` | Leave out `check_run`. |
| `--result-limit BYTES` | Longest tool result, 4096 to 4194304 (default 65536). Longer results are cut and say so. |

Secrets stay in the files these flags name, never in the client
configuration. If the server cannot start, the error appears in the tool's MCP
log.

### Client configuration

Claude Code reads `.mcp.json` in a project:

```json
{
  "mcpServers": {
    "owngit": {
      "command": "owngit",
      "args": ["mcp", "--workdir", "/path/to/clone", "--credential-file", "/path/to/helper-token"]
    }
  }
}
```

Codex reads `~/.codex/config.toml`. Codex cancels a tool call after 60
seconds by default, which stops a running `check_run`, so set
`tool_timeout_sec` above the time your checks take:

```toml
[mcp_servers.owngit]
command = "owngit"
args = ["mcp", "--workdir", "/path/to/clone", "--credential-file", "/path/to/helper-token"]
tool_timeout_sec = 1800
```

Other clients: use the stdio transport, the command `owngit` (or its full
path) and the arguments `mcp` plus the flags above.

### Tools

Read tools change nothing:

| Tool | Same as |
| --- | --- |
| `repository_list`, `repository_show` | `repo list`, `repo show` |
| `repository_kept_history`, `repository_restore_preview` | `repo kept-history`, `repo restore preview` |
| `pull_request_list`, `pull_request_show` | `pr list`, `pr show` |
| `pull_request_diff`, `pull_request_mergeability` | `pr diff`, `pr mergeability` |
| `check_task_list`, `check_status`, `check_log`, `check_cycle_list`, `check_config_show` | the `check` commands of the same name |
| `activity` | `activity` |
| `backup_status` | A summary of the backup records: schedule, last run, last verified backup, next run. It names no folder or repository. |

Write tools:

| Tool | Effect |
| --- | --- |
| `pull_request_create`, `pull_request_edit` | Add a pull request, or change its title or description. |
| `pull_request_review`, `pull_request_review_request`, `pull_request_review_skip` | Record a review decision or state for exact commits. Advisory. |
| `pull_request_close`, `pull_request_reopen` | Change the pull request state. No branch moves. |
| `pull_request_merge` | Merge into the target branch. Refused if a branch moved. |
| `repository_restore_apply` | Apply a restore preview as one new commit. Never rewrites history. |
| `check_task_create`, `check_cycle_reserve` | Add a task, or use one of its correction rounds. |
| `check_run` | Run the committed checks in `--workdir` and record the attempt. |

The MCP server offers no administrator commands, no credential management, no
repository creation and no `--check`.

### Safety

- `check_run` runs commands committed in the repository, with your
  permissions and no sandbox. Anyone who can commit to the clone can make it
  start a program. If an agent may edit files but must not run commands, start
  the server with `--no-run-check`.
- Titles, descriptions, review notes, branch names, paths, patches and check
  output come from repository users. The tool descriptions tell the agent to
  treat them as data and not to follow instructions in them.
- Only one `check_run` runs at a time. Calls other than `check_run` stop
  after 2 minutes.

## Limits

- Checks are advisory. They never block a merge, and a pass does not prove
  the code is correct.
- The helper is not a sandbox. A check can read any file and credential your
  account can.
- A `--no-upload` run is not recorded. Do not report it as server evidence.
- Do not weaken or replace the committed check configuration to make a check
  pass.
