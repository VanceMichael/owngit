# 코딩 도구: Codex, Claude Code 같은 에이전트를 OwnGit에 연결하기

<p align="center"><a href="CODING_TOOLS.md">English</a> | <b>한국어</b></p>

Codex, Claude Code, Pi 같은 코딩 도구가 OwnGit 서버를 쓰게 하는 방법을 설명합니다. 연결하면 코딩 도구가 저장소 목록을 보고 풀 리퀘스트를 열어 리뷰하고 병합할 수 있습니다. 프로젝트 체크 결과도 기록할 수 있습니다.

코딩 도구가 OwnGit을 쓰는 방법은 두 가지입니다.

- `owngit` 명령을 실행하고 출력된 JSON을 읽습니다. 어떤 명령을 실행할지는 [owngit-checks 스킬](../integrations/skills/owngit-checks/SKILL.md)이 알려 줍니다.
- MCP(Model Context Protocol)를 지원하는 도구는 [`owngit mcp`](#mcp-서버)를 띄우고 같은 명령을 도구로 호출합니다.

명령은 JSON을 출력합니다. 실패하면 바뀌지 않는 `error.code`와 무엇을 해야 하는지 알려 주는 메시지가 나옵니다.

## 빠른 시작

1. 대시보드 사이드바에서 **코딩 도구**(`/coding-tools`)를 엽니다. 이 서버의 주소와 아래 명령이 복사할 수 있게 나옵니다.
2. 코딩 도구가 스킬을 찾는 곳에 스킬을 설치합니다.

   ```sh
   owngit skill --install ~/.agents/skills
   ```

3. MCP를 쓰려면 코딩 도구에 서버를 추가합니다.

   ```sh
   claude mcp add --scope user owngit -- owngit mcp --server https://owngit.example.test
   codex mcp add owngit -- owngit mcp --server https://owngit.example.test
   ```

   서버가 비밀번호를 요구하면 화면의 명령에 `--password-file`이 붙습니다. 체크 도구까지 쓰려면 `--credential-file`도 붙이세요.

4. 체크를 기록하려면 [체크 에이전트 토큰을 만듭니다](#체크-에이전트-토큰).

OwnGit 저장소의 클론 안에서는 `--server`와 `--repository`를 생략할 수 있습니다. [클론 안에서 실행하기](#클론-안에서-실행하기)를 보세요.

## owngit 명령 준비

Homebrew, npm, Arch Linux 패키지는 `owngit`을 `PATH`에 넣습니다. 한 줄 설치 스크립트, 포터블 압축 파일, 소스 빌드는 `PATH`를 바꾸지 않습니다. 이때는 셸 시작 파일을 고치지 말고 코딩 도구에 프로그램의 전체 경로를 알려 주세요.

## 스킬 설치

`owngit skill --install DIR`은 `DIR/owngit-checks/SKILL.md`를 씁니다. `owngit` 실행 파일마다 함께 배포된 스킬이 들어 있고 포터블 압축 파일에도 `integrations/skills/owngit-checks/SKILL.md`로 들어 있습니다.

설치된 파일을 직접 고쳤다면 명령은 아무것도 바꾸지 않고 `skill_modified`로 멈춥니다. `owngit skill --print`로 비교한 다음 `--replace`를 붙이세요. 이전 파일은 새 파일 옆에 남습니다.

도구별로 스킬을 찾는 위치입니다.

| 도구 | 프로젝트 | 사용자 전체 |
| --- | --- | --- |
| Codex | `.agents/skills/` | `~/.agents/skills/` |
| Pi | `.agents/skills/` 또는 `.pi/skills/` | `~/.agents/skills/` 또는 `~/.pi/agent/skills/` |

설명만으로는 스킬이 골라지지 않을 수 있으니 이름으로 부르세요. Codex 앱에서는 `@`로 고르고 Codex CLI에서는 `$`로 지정합니다. Pi에서는 `/skill:owngit-checks`를 씁니다.

## 접근과 자격 증명

OwnGit의 비밀 값은 세 가지입니다. 공용 비밀번호는 서버의 모든 사용자가 로그인할 때 쓰는 비밀번호입니다(일반 접근). 비밀 값은 모두 내 계정만 읽을 수 있는 파일에 두고 명령 인수나 문서, 로그에는 넣지 마세요.

| 비밀 값 | 쓰는 곳 | 플래그 |
| --- | --- | --- |
| 공용 비밀번호(일반 접근) | `owngit pr`, `owngit repo list`, `show`, `create`, `owngit tasks`, 대부분의 MCP 도구 | `--password-file`(접근이 열려 있으면 생략) |
| 체크 에이전트 토큰(helper credential) | `owngit check`와 MCP 체크 도구, 저장소 하나에만 유효 | `--credential-file` |
| 관리자 비밀번호 | `helper-credential`, `check-policy`, `check-job`, 관리자용 `repo` 명령 | `--password-file` |

암호화되지 않은 HTTP는 그 명령에 `--accept-insecure-http`를 붙여야만 받아들입니다. 연결이 암호화되지 않는다는 점을 받아들일 때만 붙이세요. 코딩 도구가 사용자 대신 붙여서는 안 됩니다.

### 체크 에이전트 토큰

체크 에이전트 토큰이 있으면 코딩 도구가 저장소 하나의 체크를 기록할 수 있습니다. 관리자 비밀번호로 만듭니다.

```sh
owngit helper-credential create \
  --server https://owngit.example.test \
  --repository example-project \
  --label laptop \
  --password-file /path/to/admin-password-file \
  --output /path/to/helper-token
```

- 토큰은 새로 만든 `--output` 파일에만 쓰입니다. 서버는 해시만 보관하므로 토큰을 잃어버리면 다시 볼 수 없습니다. 새로 만드세요.
- `--output` 위치에 파일이 이미 있으면 덮어쓰지 않습니다.
- `owngit helper-credential list`와 `revoke --id ID`로 토큰을 관리합니다. 저장소의 체크 탭에서 여는 **체크 에이전트 토큰** 화면에서도 같은 일을 할 수 있습니다. 폐기한 토큰은 바로 쓸 수 없게 됩니다.
- 관리자로 확인된 브라우저에서는 코딩 도구 화면에 모든 저장소의 토큰이 나옵니다. 라벨은 만들 때 붙인 이름일 뿐, 누가 썼는지 보여 주지는 않습니다.

### 자격 증명 파일과 서버 줄

비밀번호 파일이나 토큰 파일의 첫 줄에 그 파일을 쓸 서버를 적을 수 있습니다.

```text
owngit-server: https://owngit.example.test
SECRET
```

이런 파일은 적힌 서버에만 보냅니다. `helper-credential create`와 `runner-credential issue`는 이 줄을 알아서 씁니다. 이 줄이 없는 파일은 `--server`를 직접 줄 때만 쓸 수 있습니다.

OwnGit이 클론의 `origin` 원격에서 서버를 읽을 때는 파일에 이 줄이 꼭 있어야 합니다. 클론의 `origin`은 어디든 가리킬 수 있으므로, 그 서버를 믿는다는 표시가 바로 이 줄입니다.

macOS나 Linux에서 비밀번호 파일에 이 줄을 붙이려면 나만 읽을 수 있는 새 파일을 만드세요.

```sh
(umask 077; { printf 'owngit-server: %s\n' https://owngit.example.test; cat password-file; } > bound-password-file)
```

Windows에서 메모장이나 `echo`로 만든 파일은 다른 계정도 읽을 수 있어 거부됩니다. PowerShell(5.1 또는 7)에서 내 계정만 읽을 수 있는 파일을 만들고 프롬프트에 비밀번호를 입력한 뒤 서버 줄을 붙이세요.

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

토큰 파일을 직접 읽는 스크립트는 마지막 줄을 써야 합니다.

## 클론 안에서 실행하기

OwnGit 저장소의 클론 안에서는 `owngit pr`, `owngit check`, `owngit repo`가 빠진 `--server`와 `--repository`를 `origin` 원격에서 읽습니다. `origin`은 OwnGit 클론 주소(`http(s)://HOST[:PORT]/git/NAME.git`)여야 합니다. 직접 준 플래그가 항상 우선합니다. 명령은 무엇을 읽었는지 표준 오류에 한 줄로 알려 줍니다.

`check run`은 `--workdir`가 속한 클론을 읽습니다. 나머지 명령은 현재 디렉터리가 속한 클론을 읽습니다.

저장소 이름이 바뀌면 `owngit pr`와 `owngit repo`는 예전 주소에서 `repository_moved`로 멈추고 `details.address`에 새 이름이 나옵니다. 체크 명령은 예전 주소로도 90일 동안 동작합니다. 클론의 주소를 바꾸세요.

```sh
git remote set-url origin https://owngit.example.test/git/new-name.git
```

## 저장소

`owngit repo list`, `show`, `create`는 일반 접근을 씁니다.

```sh
owngit repo list --server https://owngit.example.test
owngit repo show --server https://owngit.example.test --repository example-project
owngit repo create --server https://owngit.example.test --name example-project \
  --description "Optional description"
```

이름 바꾸기, 삭제, 설정, 기본 브랜치, 공유 링크 같은 관리자 명령과 이전 커밋에서 파일을 되돌리는 방법은 [저장소](REPOSITORIES.ko.md)에 있습니다.

## 풀 리퀘스트

푸시만으로는 풀 리퀘스트가 열리지 않습니다. 브랜치를 푸시한 뒤 만드세요.

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

`--body-file -`는 Markdown 설명을 표준 입력에서 읽습니다. `--review skip`은 리뷰를 일부러 건너뛰었다고 기록할 뿐, 승인이 아닙니다.

그다음부터는 번호로 다룹니다. 리뷰와 병합 명령에는 `pr show`가 출력한 원본과 대상 커밋을 그대로 넘깁니다.

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

알아 둘 점:

- 원본과 대상 브랜치 쌍마다 열린 풀 리퀘스트는 하나뿐입니다. 두 번째는 `pull_request_exists`로 거부합니다.
- 읽은 뒤에 브랜치가 움직였다면 리뷰와 병합은 `stale_revision`으로 실패합니다. 풀 리퀘스트를 다시 읽고 새 커밋을 기준으로 판단하세요.
- `pr edit`에는 `pr show`가 준 `edit_revision`이 필요합니다. 그사이 누가 고쳤다면 `stale_edit`로 실패합니다. 풀 리퀘스트를 다시 보고 변경을 새로 적용하세요.
- `pr diff`는 병합 기준(merge base)부터 원본까지의 변경을 보여 줍니다. `--stat`은 패치를 빼고 `--patch`는 패치만 출력합니다. 큰 diff는 파일 단위로 잘리고 결과에 `truncated`로 표시됩니다.
- 서버가 텍스트로 비교하기에 너무 큰 파일은 `too_large: true`로 표시되고 `additions`, `deletions`, 패치 구간이 모두 빠집니다. 패치에서 빠진 것이 이런 파일뿐이면 `reason`은 `too_large`이고 `pr diff --patch`는 빠진 파일 이름을 안내 줄로 알려 줍니다. `repo restore preview`의 변경 목록도 이런 파일을 같은 방식으로 표시합니다. 줄 수를 읽지 않은 파일에도 `additions`와 `deletions`가 없습니다. 한 변경에 너무 큰 파일이 100개를 넘을 때가 그런 경우입니다.
- `pr mergeability`는 `clean`, `conflict`, `unavailable`, `stale` 중 하나로 답합니다. 아무것도 바꾸지 않으며 `pr merge`는 실행할 때 다시 확인합니다.
- 병합은 빨리 감기(fast-forward)이거나 `OwnGit <owngit@localhost>`가 만든 병합 커밋입니다. 스쿼시나 리베이스를 하지 않고 원본 브랜치도 지우지 않습니다. 다시 시도해도 커밋이 두 번 생기지 않습니다. 서버에 Git 2.38 이상이 있어야 병합할 수 있습니다.
- 리뷰와 체크는 참고용입니다. 어느 쪽도 병합을 막지 않습니다.
- `pr list`는 최신순으로 50개씩 보여 줍니다. 더 남아 있으면 결과에 `next`가 있으니 `--before`로 넘겨 다음 페이지를 받으세요.

### 자동 병합

검사가 끝날 때까지 화면을 지켜보다가 직접 병합하는 대신, 열린 풀 리퀘스트에 자동 병합을 켤 수 있습니다. 그러면 현재 원본 커밋의 검사를 통과하는 시점에 OwnGit이 직접 병합합니다.

```sh
owngit pr auto-merge --number 1
owngit pr auto-merge --number 1 --cancel
```

풀 리퀘스트 화면이나 소유자 API로도 켜거나 취소할 수 있습니다.

```sh
curl -X POST -u owngit:PASSWORD \
  https://owngit.example.test/api/v1/repositories/example-project/pull-requests/1/auto-merge
curl -X POST -u owngit:PASSWORD \
  https://owngit.example.test/api/v1/repositories/example-project/pull-requests/1/auto-merge/cancel
```

알아 둘 점:

- 이 의도는 커밋이 아니라 풀 리퀘스트에 속하므로 원본 브랜치에 푸시해도 유지됩니다. 병합 여부는 언제나 **현재** 원본과 대상 커밋을 기준으로 판단합니다.
- OwnGit이 병합하는 것은 현재 원본 커밋의 최근 검사 증거가 최종 상태로 통과했고, 그 커밋을 깨끗한 작업 트리에서 실제로 테스트했으며, 해당 커밋에 커밋된 검사 구성을 사용했고, 읽을 수 있을 때뿐입니다. 증거가 없거나 검사가 진행 중이거나, 실패했거나, 작업 트리가 깨끗하지 않거나, 구성이 바뀌었거나, 기록을 읽을 수 없으면 풀 리퀘스트에 명확한 대기 또는 차단 상태가 표시되고 병합은 게시되지 않습니다.
- 현재 커밋에 검사 증거가 전혀 없으면 검사가 구성되지 않은 경우에도 계속 기다립니다. 이후 체크 에이전트나 구성된 체크가 그 커밋의 통과 결과를 기록하면 병합합니다.
- 원본 브랜치가 움직이면 예전 통과 결과는 재사용하지 않고 새 커밋의 검사를 기다립니다. 대상 브랜치가 움직이면 현재 커밋 쌍과 병합 가능성을 다시 확인하며, 충돌은 나중 ref 변경으로 해소될 때까지 대기 상태로만 둡니다.
- 언제든 취소할 수 있습니다. 풀 리퀘스트를 닫아도 의도가 취소되며 다시 열 때 자동으로 되살아나지 않습니다. 취소나 닫기가 대기 중일 때 병합이 이미 게시되었다면 풀 리퀘스트는 병합됨으로 보고되고 두 번 병합되지 않습니다.
- 대기 중인 의도는 서비스 재시작과 백업 복원 후에도 유지됩니다. 저장소를 일시적으로 사용할 수 없으면 의도를 유지하며 다음 검사 완료나 ref 변경 때 다시 시도합니다.
- 자동 병합을 켜지 않으면 달라지는 것은 없습니다. 검사와 리뷰는 계속 참고용이고, 검사가 실패하거나 실행 중이어도 수동 `pr merge`는 이전과 똑같이 동작합니다.

## 체크 기록하기

`owngit check` 명령(체크 에이전트)은 내 컴퓨터에서 프로젝트 체크를 실행하고 테스트한 바로 그 커밋에 묶어 결과를 서버에 기록합니다. [체크 에이전트 토큰](#체크-에이전트-토큰)이 필요합니다. 푸시할 때마다 OwnGit이 직접 실행하는 체크는 [자동 체크](AUTOMATIC_CHECKS.ko.md)를 보세요.

체크는 내 계정의 권한으로 실행되며 내 파일에 접근할 수 있습니다. 샌드박스는 없습니다. 셸의 환경 변수 가운데 `PATH`, `HOME`, 로캘 설정 같은 몇 가지만 받고 프록시 설정, `JAVA_HOME` 같은 도구 설정, 인증 정보는 받지 않습니다. `TMPDIR`, `TEMP`, `TMP`는 시스템 임시 폴더 안에 새로 만든 비공개 폴더를 가리키며 실행이 끝나면 이 폴더를 지웁니다. 명령에 다른 변수가 필요하면 `HTTPS_PROXY=http://proxy.example.test:3128 go test ./...`처럼 명령 안에서 지정하세요. 체크 명령이 실패하거나 시작하지 못하면 로그 맨 앞에 받지 못한 변수의 이름이 나옵니다. 임시 폴더 오류는 이 안내 없이 따로 알립니다. 전체 목록은 [환경 변수](AUTOMATIC_CHECKS.ko.md#환경-변수)를 보세요.

1. 한 가지 일마다 작업(task)을 하나 만듭니다. 커밋이 바뀌어도 그 ID를 계속 씁니다.

   ```sh
   owngit check task new \
     --server https://owngit.example.test \
     --repository example-project \
     --credential-file /path/to/helper-token \
     --title "Fix the failing build"
   ```

2. 체크를 실행합니다. `--check`가 없으면 `--workdir`(기본값 `.`)의 `HEAD`에 커밋된 `.owngit/checks.json`의 체크를 실행합니다. `--check name=command`를 주면 준 체크만 실행합니다.

   ```sh
   owngit check run --task TASK_ID \
     --server https://owngit.example.test \
     --repository example-project \
     --credential-file /path/to/helper-token
   ```

3. 에이전트가 수정을 시도하기 전에 수정 라운드를 예약하고 수정을 확인하는 실행에 넘깁니다.

   ```sh
   owngit check cycle reserve --task TASK_ID ...
   owngit check run --task TASK_ID --cycle CYCLE_ID ...
   ```

4. 기록된 내용을 읽습니다.

   ```sh
   owngit check task list ...
   owngit check status --task TASK_ID ...
   owngit check log --attempt ATTEMPT_ID ...
   ```

`...`는 같은 `--server`, `--repository`, `--credential-file` 플래그입니다. 체크마다 기본으로 10분과 출력 64 KiB가 주어집니다(`--timeout`, `--output-limit`). `--no-upload`는 체크를 로컬에서만 실행하고 아무것도 기록하지 않습니다. 원본 로그 보관 기간은 [체크 원본 로그](AUTOMATIC_CHECKS.ko.md#체크-원본-로그)를 보세요.

커밋된 `.owngit/checks.json`을 Git이 읽는 시간은 30초까지입니다. 부분 클론(partial clone, `--filter`로 만든 클론)에서는 이 파일이 아직 원격에만 있을 수 있습니다. 30초 안에 읽지 못하면 실행은 `revision_unavailable`로 멈추고 아무것도 기록하지 않습니다. `REVISION` 자리에 메시지에 나온 커밋을 넣어 `git show REVISION:.owngit/checks.json`을 한 번 실행하면 파일을 내려받습니다. 그다음 체크를 다시 실행하세요.

### 결과 읽기

`check run`의 종료 코드는 다음과 같습니다.

| 종료 코드 | 뜻 |
| --- | --- |
| `0` | 모든 체크가 통과했습니다. |
| `1` | 통과하지 못한 체크가 있거나 체크를 하나도 실행하기 전에 멈췄습니다(예: `checks_not_configured`). |
| `2` | 체크는 실행했지만 서버가 기록했는지 이 클라이언트가 확인하지 못했습니다. 이유는 `upload_error`에 있습니다. |
| `128` + 시그널 번호 | 시그널로 실행이 멈췄습니다. Ctrl-C는 `130`, 터미널을 닫으면 `129`, `SIGTERM`은 `143`입니다. |

시그널을 받으면 체크는 멈춥니다. 하지만 서버 등록이 이미 시작됐다면 클라이언트는 등록과 완료 기록을 끝까지 진행하므로 실행이 기록됩니다. 체크가 이미 통과한 뒤였어도 종료 코드는 그 시그널을 가리킵니다. 다만 서버 기록을 확인하지 못하면 `2`입니다. 시그널이 오기 전에 체크가 통과하지 못한 채 끝났거나 `error`로 끝난 체크가 있으면 `1`입니다. 다음 경우는 다릅니다.

- 서버 등록을 시작하기 전, 준비 단계에서 시그널을 받으면 그 자리에서 멈춥니다. 체크는 실행되지 않고 기록도 결과 출력도 남지 않습니다.
- 시그널을 한 번 더 보내면 등록이나 완료 기록을 기다리지 않고 바로 끝냅니다. 이때 종료 코드는 두 번째 시그널을 가리킵니다. 서버가 실행을 이미 기록했을 수 있습니다. 무엇이 남았는지는 `check status --task TASK_ID`로 확인하세요.
- `nohup`처럼 `SIGHUP`을 무시하도록 시작한 실행은 터미널을 닫아도 멈추지 않으며 체크 결과에 맞는 종료 코드로 끝납니다.

체크마다 결과는 `passed`, `failed`, `error`, `cancelled`, `incomplete`(시간이나 출력 한도를 넘음), `unavailable` 중 하나입니다.

`attempt.worktree_state`는 클론이 깨끗하게 남았는지 알려 줍니다. 클라이언트는 체크 전후에 Git으로 워킹 트리를 읽으며 한 번 읽을 때마다 30초까지 기다립니다. 체크가 추적 중인 파일을 바꿨거나 커밋을 옮겼으면 `dirty`입니다. 읽기에 실패했거나 30초 안에 끝나지 않았거나 시그널로 멈췄으면 `unknown`입니다. 그래서 체크 도중에 멈춘 실행은 이미 변경을 발견한 경우가 아니면 `unknown`으로 기록됩니다. 상태가 `unknown`이면 결과의 `worktree_note`에 이유가 나옵니다. 문구는 기록된 로그의 첫 줄과 같습니다. 상태를 읽었으면 이 필드는 없습니다. `dirty`나 `unknown`인 커밋을 테스트했다고 말하지 마세요.

파일이 이미 캐시에 올라와 있는 보통 크기의 체크아웃이라면 30초는 넉넉합니다. 아주 큰 체크아웃을 캐시가 비어 있는 상태에서 읽으면, 특히 Windows에서는 30초를 넘겨 `unknown`이 될 수 있습니다.

### 체크가 시작한 프로세스

체크가 끝나면 클라이언트는 그 체크가 시작한 프로그램을 멈춥니다. 정상적으로 끝났을 때도, 시간 한도에 걸렸을 때도, 중간에 멈췄을 때도 마찬가지입니다. 클라이언트가 직접 실행하는 Git 읽기도 똑같이 처리합니다. 데이터베이스나 개발 서버처럼 오래 떠 있어야 하는 프로그램은 OwnGit 밖에서 시작하세요.

어디까지 멈추는지는 운영체제마다 다릅니다.

- Linux: 체크가 시작한 프로세스를 모두 멈춥니다. `setsid` 등으로 새 세션을 시작한 프로세스도 포함됩니다.
- macOS와 그 밖의 Unix: 체크의 프로세스 그룹만 멈춥니다. 새 세션을 시작한 프로세스는 계속 실행됩니다.
- Windows: 잡 객체(job object)가 체크의 프로세스를 모두 묶고 있으므로 모두 멈춥니다.

Linux에서 클라이언트는 체크의 환경 변수에 표시를 남기고 `/proc`에서 그 표시를 읽어 프로세스 그룹을 벗어난 프로세스를 찾습니다. 환경 변수를 지운 프로세스, 다른 계정으로 실행되는 프로세스, 덤프 불가(not dumpable)로 표시된 프로세스는 찾지 못합니다. 덤프 불가 프로세스는 커널이 환경 변수를 숨겨서 읽을 수 없습니다. 시스템에 `/proc`이 없으면 프로세스 그룹만 멈추고 로그를 한 줄 남깁니다.

정리 시간(Linux에서는 약 5초) 안에 체크가 시작한 프로세스를 OwnGit이 모두 끝내지 못하거나, 프로세스를 찾으려고 `/proc`을 읽지 못하면 그 체크는 `error`로 끝납니다. 체크 결과의 `cleanup_error` 필드에 둘 중 어느 쪽인지 나옵니다.

### 수정 라운드

작업마다 수정 라운드는 세 번입니다. 자동 수정을 할 때마다 먼저 하나를 예약하세요. 첫 실행과 수동 재실행은 라운드를 쓰지 않습니다. 라운드가 남지 않으면 `check cycle reserve`가 `correction_budget_exhausted`로 실패합니다. 그때는 멈추고 끝나지 않은 작업을 보고하세요. 수동 실행은 그 뒤에도 기록할 수 있습니다.

남은 라운드는 `check status`나, 기록된 실행 결과의 `task` 객체에서만 읽으세요. 맨 위의 `correction_cycles_remaining`은 클라이언트가 서버 응답을 받지 못하면 `0`으로 나옵니다. 그런 경우는 두 가지입니다.

- `--no-upload`는 서버에 연결하지 않습니다.
- 등록에 실패한 실행(`registered`가 false이고 `upload_error`가 있음)은 확인되지 않은(unconfirmed) 상태입니다. 서버가 그 시도를 기록했을 수도, 기록하지 않았을 수도 있습니다.

확인되지 않은 시도라면 작업 ID는 그대로 두고 출력된 `attempt_id`를 진단 증거(diagnostic evidence)로 남긴 뒤 `check status --task TASK_ID`를 실행하세요. 확인하려고 체크를 다시 실행하거나 라운드를 예약하지 마세요. 다시 실행하면 언제나 새 시도가 됩니다. `check status`에 그 시도가 나오지 않으면 확인되지 않았다고 보고하세요. 이 `0`을 라운드가 바닥났다는 뜻으로 받아들이면 안 됩니다.

## MCP 서버

`owngit mcp`는 로컬 MCP 서버입니다. 표준 입력과 출력(stdio 전송)으로 통신하며 네트워크 포트를 열지 않습니다. 도구 하나가 `owngit` 명령 하나를 실행하고 그 JSON을 돌려줍니다. 셸 명령을 실행할 수 있는 도구라면 명령줄을 그대로 쓰는 편이 대개 토큰을 덜 씁니다.

### 시작 옵션

서버는 코딩 도구가 띄웁니다. 도구가 접근할 수 있는 범위는 다음 플래그로 정해집니다.

| 플래그 | 뜻 |
| --- | --- |
| `--workdir DIR` | `origin`으로 서버와 저장소를 알려 주는 클론이자 `check_run`이 실행되는 곳. 절대 경로로 주세요. |
| `--server`, `--repository` | `origin` 대신 씁니다. 저장소를 정하지 않으면 도구가 `repository` 인수를 받습니다. |
| `--password-file` | 공용 비밀번호. |
| `--credential-file` | 체크 에이전트 토큰. 체크 도구가 추가됩니다. |
| `--accept-insecure-http` | 암호화되지 않은 HTTP 서버를 허용합니다. |
| `--no-run-check` | `check_run`을 뺍니다. |
| `--result-limit BYTES` | 도구 결과 하나의 최대 크기. 4096부터 4194304까지이며 기본값은 65536입니다. 넘는 결과는 잘리고 잘렸다고 표시됩니다. |

비밀 값은 이 플래그가 가리키는 파일에만 두고 클라이언트 설정에는 넣지 마세요. 서버가 시작하지 못하면 오류는 코딩 도구의 MCP 로그에 나옵니다.

### 클라이언트 설정

Claude Code는 프로젝트의 `.mcp.json`을 읽습니다.

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

Codex는 `~/.codex/config.toml`을 읽습니다. Codex는 기본적으로 60초가 지나면 도구 호출을 취소합니다. 그러면 실행 중인 `check_run`도 멈춥니다. `tool_timeout_sec`을 체크에 걸리는 시간보다 길게 잡으세요.

```toml
[mcp_servers.owngit]
command = "owngit"
args = ["mcp", "--workdir", "/path/to/clone", "--credential-file", "/path/to/helper-token"]
tool_timeout_sec = 1800
```

다른 클라이언트는 stdio 전송, 명령 `owngit`(또는 전체 경로), 인수 `mcp`와 위 플래그를 쓰면 됩니다.

### 도구

읽기 도구는 아무것도 바꾸지 않습니다.

| 도구 | 해당 명령 |
| --- | --- |
| `repository_list`, `repository_show` | `repo list`, `repo show` |
| `repository_kept_history`, `repository_restore_preview` | `repo kept-history`, `repo restore preview` |
| `pull_request_list`, `pull_request_show` | `pr list`, `pr show` |
| `pull_request_diff`, `pull_request_mergeability` | `pr diff`, `pr mergeability` |
| `check_task_list`, `check_status`, `check_log`, `check_cycle_list`, `check_config_show` | 이름이 같은 `check` 명령 |
| `activity` | `activity` |
| `backup_status` | 백업 기록 요약: 예약 상태, 마지막 실행, 마지막으로 검증된 백업, 다음 실행. 폴더나 저장소 이름은 나오지 않습니다. |

쓰기 도구와 그 효과입니다.

| 도구 | 하는 일 |
| --- | --- |
| `pull_request_create`, `pull_request_edit` | 풀 리퀘스트를 만들거나 제목과 설명을 고칩니다. |
| `pull_request_review`, `pull_request_review_request`, `pull_request_review_skip` | 정확한 커밋에 대해 리뷰 결정이나 상태를 기록합니다. 참고용입니다. |
| `pull_request_close`, `pull_request_reopen` | 풀 리퀘스트 상태를 바꿉니다. 브랜치는 움직이지 않습니다. |
| `pull_request_merge` | 대상 브랜치에 병합합니다. 브랜치가 움직였으면 거부합니다. |
| `repository_restore_apply` | 되돌리기 미리보기를 새 커밋 하나로 적용합니다. 기록을 다시 쓰지 않습니다. |
| `check_task_create`, `check_cycle_reserve` | 작업을 만들거나 수정 라운드를 하나 씁니다. |
| `check_run` | `--workdir`에 커밋된 체크를 실행하고 시도를 기록합니다. |

MCP 서버에는 관리자 명령, 토큰 관리, 저장소 생성, `--check`가 없습니다.

### 안전

- `check_run`은 저장소에 커밋된 명령을 내 권한으로, 샌드박스 없이 실행합니다. 클론에 커밋할 수 있는 사람은 누구나 이 도구로 프로그램을 실행시킬 수 있습니다. 에이전트가 파일은 고치되 명령은 실행하면 안 된다면 `--no-run-check`로 서버를 띄우세요.
- 제목, 설명, 리뷰 메모, 브랜치 이름, 경로, 패치, 체크 출력은 저장소 사용자가 쓴 내용입니다. 도구 설명은 에이전트에게 이것을 데이터로만 다루고 그 안의 지시를 따르지 말라고 알려 줍니다.
- `check_run`은 한 번에 하나만 실행됩니다. `check_run`이 아닌 호출은 2분이 지나면 멈춥니다.

## 제한

- 체크는 참고용입니다. 병합을 막지 않고 통과했다고 코드가 옳다는 증거가 되지는 않습니다.
- 체크 에이전트는 샌드박스 없이 실행됩니다. 체크는 내 계정이 읽을 수 있는 파일과 자격 증명을 모두 읽을 수 있습니다.
- `--no-upload` 실행은 기록되지 않습니다. 서버에 남은 증거처럼 보고하지 마세요.
- 체크를 통과시키려고 커밋된 체크 설정을 약하게 하거나 바꾸지 마세요.
