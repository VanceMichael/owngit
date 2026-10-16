# 자동 체크

<p align="center"><a href="AUTOMATIC_CHECKS.md">English</a> | <b>한국어</b></p>

자동 체크를 켜면 누가 푸시하거나 풀 리퀘스트를 갱신할 때 OwnGit이 저장소의 테스트를 직접 실행합니다. 이 안내는 서버 소유자를 위한 것입니다.

켜는 순서입니다.

1. 저장소에 [체크 파일](#체크-파일) `.owngit/checks.json`을 커밋합니다.
2. [체크를 실행할 곳](#체크를-실행할-곳)을 고르고 [정책](#정책과-체크-켜기)을 저장합니다.
3. 그 정책으로 체크를 켭니다.

결과는 저장소의 체크 탭과 각 풀 리퀘스트에 나옵니다. 결과는 참고용이며 병합을 막지 않습니다. 코딩 도구에서 내 컴퓨터로 체크를 실행하려면 [코딩 도구](CODING_TOOLS.ko.md)를 보세요.

수동 병합에서 체크는 계속 참고용입니다. 열린 풀 리퀘스트에 별도로 자동 병합을 켜면, 현재 원본 커밋의 검사가 깨끗한 작업 트리에서 최종 상태로 통과하고 현재 커밋 쌍이 충돌 없이 병합될 때 OwnGit이 그 요청을 직접 병합합니다. 코딩 도구 안내의 [자동 병합](CODING_TOOLS.ko.md#자동-병합)을 보세요.

## 체크 파일

무엇을 언제 실행할지는 저장소가 정합니다. OwnGit은 체크하는 바로 그 커밋에서 이 파일을 읽습니다.

```json
{
  "version": 1,
  "events": {
    "push": {"branches": ["main", "release/*"]},
    "pull_request": {}
  },
  "checks": [{"name": "unit", "command": "go test ./..."}],
  "limits": {"timeout_ms": 600000, "output_limit_bytes": 65536}
}
```

- `events`로 `push`, `pull_request`, 또는 둘 다 켭니다. `"pull_request": {}`처럼 이벤트에 빈 객체를 주면 모든 브랜치가 대상입니다. 브랜치 패턴은 이름 그대로이거나 이름 끝에 `*`가 하나 붙은 형태입니다.
- `checks`에는 이름이 붙은 셸 명령을 1개부터 50개까지 넣습니다.
- `limits`는 넣지 않아도 됩니다. 파일에서 따로 정하지 않으면 체크마다 10분과 출력 64 KiB가 주어집니다. 정책의 최댓값을 넘는 값은 OwnGit이 낮춥니다.
- 파일은 64 KiB를 넘을 수 없습니다. 모르는 필드나 잘못된 JSON은 거부합니다.

체크 파일로는 실행할 곳, 컨테이너 이미지, 네트워크, 정책의 어떤 한도도 정할 수 없습니다. 이런 것은 관리자만 정합니다.

## 정책과 체크 켜기

관리자의 정책은 체크를 실행할 곳, 체크를 시작할 수 있는 이벤트, 한도를 정합니다. 브라우저에서는 저장소의 체크 탭이나 설정 탭에서 여는 **자동 체크** 화면에서 고칩니다. 이 화면은 다음에 할 일과 복사해 쓸 체크 파일, 한도마다 범위를 보여 줍니다. 바꿀 때는 관리자 비밀번호를 묻습니다.

**저장하고 체크 켜기**는 저장하기 전에 바뀔 설정을 하나씩 먼저 보여 줍니다. 그냥 **저장**은 체크를 켜지 않습니다.

명령줄에서는 정책을 파일로 씁니다.

```json
{
  "executor": "host",
  "allowed_events": ["push", "pull_request"],
  "max_timeout_ms": 600000,
  "max_output_limit_bytes": 1048576,
  "queue_limit": 32,
  "max_active_jobs": 1,
  "max_lease_ms": 60000,
  "execution": {"source": {}}
}
```

`queue_limit`은 기다릴 수 있는 작업 수, `max_active_jobs`는 동시에 실행할 작업 수입니다. `max_lease_ms`는 실행을 맡은 쪽이 아무 신호를 보내지 않아도 작업을 계속 맡고 있을 수 있는 시간입니다. `max_timeout_ms`와 `max_output_limit_bytes`는 체크 파일이 요청할 수 있는 최댓값입니다.

그다음 저장과 켜기를 한 번에 합니다.

```sh
owngit check-policy set --enable \
  --server https://git.example.test \
  --repository project \
  --password-file ./admin-password \
  --policy-file ./check-policy.json
```

`check-policy show`, `enable`, `disable`도 같은 `--server`, `--repository`, `--password-file` 플래그를 씁니다.

- 체크를 켜면 저장된 그 정책만 승인합니다. 다른 정책을 저장하면 다시 켤 때까지 체크가 꺼집니다.
- 맞는 체크 파일과 승인된 정책이 둘 다 있어야 실행됩니다.
- `check-policy show`는 지금 이 컴퓨터가 체크를 실행할 수 있는지도 알려 줍니다. `workspace_unavailable`이나 `restart_reconciliation_unavailable`이 나오면 알려 준 원인을 고치고 OwnGit을 다시 시작하세요. 그동안에도 Git과 병합은 그대로 됩니다.
- 정책의 최댓값은 이 컴퓨터의 체크 상한을 넘을 수 없습니다. 상한은 관리자가 설정에서 정합니다. [운영 안내](OPERATIONS.ko.md)를 보세요.

## 체크를 실행할 곳

`executor`는 셋 중 하나입니다.

| executor | 실행 주체 | 쓰는 경우 |
| --- | --- | --- |
| `host` | OwnGit 계정, 플랫폼 셸로 실행 | 완전히 믿는 저장소 |
| `container` | 이 컴퓨터 Docker의 제한된 컨테이너 | 덜 믿는 코드, 자원 한도 적용 |
| `external_runner` | 다른 컴퓨터에서 띄운 러너 | OwnGit 컴퓨터 밖에서 체크를 돌릴 때 |

### 호스트

호스트 방식은 샌드박스 없이 실행됩니다. 체크는 OwnGit 계정이 접근할 수 있는 모든 것, 곧 OwnGit의 데이터와 비밀 값에도 접근할 수 있습니다. 커밋하는 사람을 모두 믿는 저장소에만 쓰세요. 호스트 체크는 서버의 환경 변수 가운데 일부만 받습니다. [환경 변수](#환경-변수)를 보세요.

호스트 방식과 `owngit runner`에서는 체크가 끝나면 그 체크가 시작한 프로그램을 OwnGit이 멈춥니다. Linux에서는 새 세션을 시작한 프로그램도 멈춥니다. 오래 실행해야 하는 서비스는 OwnGit 밖에서 시작하세요. 운영체제별 동작은 [체크가 시작한 프로세스](CODING_TOOLS.ko.md#체크가-시작한-프로세스)를 보세요.

### 컨테이너

컨테이너 방식에는 이 컴퓨터에서 Linux 컨테이너를 실행하는 Docker 데몬이 필요합니다. 원격 Docker는 거부하므로 OwnGit에 `DOCKER_HOST`, `DOCKER_CONTEXT`, Docker TLS 변수를 설정하지 마세요. `"executor": "container"`로 정하고 `execution`에 이미지와 자원 한도를 넣습니다.

```json
"execution": {
  "source": {},
  "container_image": "registry.example/checks@sha256:<64 hex digits>",
  "container_network": "none",
  "container_cpu_millis": 1000,
  "container_memory_bytes": 536870912,
  "container_pids": 256,
  "container_scratch_bytes": 536870912
}
```

CPU, 메모리, 프로세스, 임시 공간 값은 기본값입니다. 네트워크는 `none`이나 `bridge`입니다.

체크는 다음 조건으로 실행됩니다.

- 이미지의 사용자가 아니라 OwnGit 전용의 root가 아닌 사용자로 실행합니다.
- 루트 파일 시스템은 읽기 전용입니다. Linux 권한(capability)을 모두 빼고 `no-new-privileges`를 켭니다.
- CPU, 메모리, 프로세스 한도를 걸고 스왑은 쓰지 않습니다.
- 작업 파일만 `/workspace`에 마운트하고 `/tmp`는 크기를 제한합니다.

특권 모드, 호스트 네트워크, Docker 소켓, 호스트 마운트는 없습니다. 볼륨을 선언한 이미지와 한도를 강제하지 못하는 Docker는 거부합니다.

#### 컨테이너 설정

각 설정은 켜기 전까지 꺼져 있습니다. 켜면 체크가 할 수 있는 일이 늘어나니 무엇을 허용하는지 읽고 켜세요.

| 필드 | 허용하는 것 |
| --- | --- |
| `container_allow_tags` | 다이제스트 대신 `registry.example/checks:1` 같은 태그. 태그는 다음번에 다른 코드를 가리킬 수 있습니다. 작업마다 실제로 쓴 이미지 ID를 기록합니다. |
| `container_pull_missing` | 없는 이미지를 작업 전에 내려받기. 저장된 레지스트리 로그인을 쓰지 않으므로 공개 이미지만 됩니다. |
| `container_network`(다른 이름) | 직접 만든 Docker 네트워크. 체크가 그 네트워크의 모든 서비스에 접속할 수 있습니다. `host`는 절대 받지 않습니다. |
| `container_image_volumes` | 볼륨을 선언한 이미지 실행. 볼륨마다 메모리 안의 임시 공간을 줍니다. |
| `container_writable_root` | 명령이 컨테이너 자체 파일을 바꾸도록 허용. 바뀐 내용은 컨테이너와 함께 버립니다. |
| `container_missing_enforcement` | 이 컴퓨터의 Docker가 강제하지 못해도 되는 한도 목록(`memory`, `swap`, `cpu`, `pids`). |

이 설정을 쓴 작업은 출력 맨 앞에 이미지 ID와 Docker가 강제하지 못한 한도를 적습니다.

### 외부 러너

러너는 다른 컴퓨터에서 띄우는 `owngit runner`입니다. 러너는 체크를 자기 계정으로, 샌드박스 없이 실행하므로 전용 계정을 주세요.

1. `"executor": "external_runner"`로 정책을 저장한 뒤, 저장소용 토큰을 새 비공개 파일로 발급합니다.

   ```sh
   owngit runner-credential issue \
     --server https://git.example.test \
     --repository project \
     --password-file ./admin-password \
     --label build-host \
     --token-file ./runner-token
   ```

2. 러너를 시작합니다.

   ```sh
   owngit runner \
     --server https://git.example.test \
     --repository project \
     --token-file ./runner-token
   ```

러너는 OwnGit의 HTTPS 주소가 있어야 합니다. tailnet 공유나 리버스 프록시를 쓰세요([운영 안내](OPERATIONS.ko.md) 참고). 사설 인증 기관은 `--ca-file`로 추가합니다.

- 러너는 자기 계정의 캐시 폴더 안에서 작업합니다. 다른 곳을 쓰려면 그 계정이 소유한 빈 폴더를 `--workspace-root`로 주세요.
- OwnGit이 다시 시작하거나 네트워크가 끊겨도 러너는 계속 다시 시도합니다. 토큰이 폐기된 경우처럼 다시 시도해도 소용없을 때만 종료합니다.
- `owngit runner-credential list`와 `revoke --credential ID`로 토큰을 관리합니다. 서버는 토큰의 해시만 보관합니다.
- 저장소 이름이 바뀌면 90일 안에 새 `--repository`로 러너를 다시 시작하세요.
- [체크가 보는 파일](#체크가-보는-파일)에 나오는 읽기 한도는 OwnGit을 실행하는 컴퓨터의 한도입니다. 러너가 파일을 받기 전에 OwnGit이 파일마다 크기와 다시 만드는 데 드는 메모리를 확인합니다. 둘 중 하나라도 한도를 넘는 파일이 있으면 HTTP 422와 `check_source_refused`로 파일 목록을 거부합니다.
- 러너가 파일을 받을 때도 OwnGit은 파일마다 다시 확인합니다. 러너를 더 큰 컴퓨터에서 돌려도 이 한도는 늘지 않습니다.

재부팅 뒤에도 러너가 돌게 하려면 서비스 관리자를 쓰세요. systemd를 쓰는 Linux의 예입니다.

```ini
[Unit]
Description=OwnGit runner for project
Wants=network-online.target
After=network-online.target

[Service]
User=owngit-runner
ExecStart=/usr/local/bin/owngit runner --server https://git.example.test --repository project --token-file /etc/owngit-runner/project-token --workspace-root /srv/owngit-runner/project
Restart=on-failure
RestartSec=60

[Install]
WantedBy=multi-user.target
```

토큰 파일은 서비스 계정만 읽을 수 있어야 합니다. 토큰을 폐기했다면 새 토큰을 발급한 뒤 서비스를 다시 시작하세요.

### 환경 변수

호스트 체크와 러너 체크는 OwnGit 서버나 러너의 환경 변수 가운데 정해진 몇 가지만 받습니다. 프록시나 도구 설정처럼 다른 값이 필요하면 체크 명령에 직접 적으세요. 코딩 도구에서 실행하는 `owngit check`도 규칙이 같습니다.

서버나 러너에 다음 변수가 있으면 체크에 넘깁니다.

- 모든 운영체제: `PATH`, `HOME`, `LANG`, `TZ`, `LC_ALL`, `LC_COLLATE`, `LC_CTYPE`, `LC_MESSAGES`, `LC_MONETARY`, `LC_NUMERIC`, `LC_TIME`
- Linux와 macOS: `USER`, `LOGNAME`, `SHELL`
- Windows: 시스템, 명령 셸, 사용자 프로필, 프로그램 폴더, 프로세서 관련 변수. 예를 들면 `SystemRoot`, `ComSpec`, `PATHEXT`, `USERPROFILE`, `APPDATA`, `ProgramFiles`, `NUMBER_OF_PROCESSORS`입니다. 이름의 대소문자는 가리지 않습니다.

OwnGit은 `CI=true`도 넣고 `TMPDIR`, `TEMP`, `TMP`가 새로 만든 비공개 임시 폴더를 가리키게 합니다. 서버와 러너에서는 이 폴더를 작업 전용 폴더 안, 복사한 파일 옆에 만듭니다. `owngit check`는 시스템 임시 폴더 안에 만듭니다. 한 번 실행하는 동안 모든 체크가 이 폴더를 함께 쓰며 마지막 체크가 끝나면 OwnGit이 폴더를 지웁니다. 폴더를 만들지 못하면 그 실행의 체크가 모두 `error`로 끝납니다. 폴더를 지우지 못하면 마지막 체크가 `error`로 끝납니다.

그 밖의 변수는 넘기지 않습니다. OwnGit 자체 설정, 대문자나 소문자로 쓴 프록시 설정(`HTTPS_PROXY`, `https_proxy` 등), `JAVA_HOME` 같은 도구 설정, 인증 정보가 모두 여기에 해당합니다. 명령에 값이 필요하면 명령 안에서 지정하세요.

- Linux와 macOS: `HTTPS_PROXY=http://proxy.example.test:3128 go test ./...`
- Windows: `set "JAVA_HOME=C:\Tools\jdk" && gradlew test`

`checks.json`에서는 역슬래시를 `\\`처럼 두 번 씁니다. 체크 파일은 저장소에 커밋되므로 비밀 값을 적으면 안 됩니다.

이런 체크의 명령이 실패하거나 시작하지 못하면 로그 맨 앞에 안내가 붙습니다. 안내에는 OwnGit이 넘기지 않은 변수의 이름이 나오고, 명령에 필요한 변수는 직접 지정하라고 알려 줍니다. 값은 보여 주지 않습니다. 이름에 `TOKEN`, `SECRET`, `PASSWORD`, `KEY`, `CREDENTIAL`이 들어 있으면 대소문자와 상관없이 목록에서 뺍니다. 목록은 8 KiB까지만 적습니다. 취소된 체크에는 안내가 붙지 않습니다. 임시 폴더를 만들거나 지우지 못하면 OwnGit은 그 오류를 따로 알리고, 그 때문에 안내를 붙이지는 않습니다.

이 규칙은 체크가 볼 변수만 정합니다. 샌드박스는 아니므로 체크는 여전히 실행 계정이 할 수 있는 모든 것을 읽고 바꿀 수 있습니다.

컨테이너 체크에는 이 목록을 쓰지 않습니다. 컨테이너 안의 명령은 이미지가 정한 변수와 OwnGit이 정한 값만 받습니다. OwnGit은 `HOME`, `TMPDIR`, `TMP`, `TEMP`, `GOTMPDIR`를 `/tmp`로, `XDG_CACHE_HOME`을 `/tmp/.cache`로, `GOCACHE`를 `/tmp/go-build`로 정합니다. `HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY`, `FTP_PROXY`, `ALL_PROXY`와 각각의 소문자 이름은 빈 값으로 정하므로 이미지나 서버의 Docker 클라이언트 설정에 있는 프록시 설정이 명령에 들어가지 않습니다. 다만 OwnGit이 직접 실행하는 `docker` 명령은 서버의 환경 변수를 그대로 씁니다. 그래서 [컨테이너](#컨테이너)에서 말한 Docker 변수가 중요합니다.

## 작업

체크 파일과 맞는 푸시나 풀 리퀘스트 갱신마다 OwnGit이 작업(job)을 만듭니다. Git은 체크를 기다리지 않습니다.

- 푸시 체크를 켜면 아직 작업이 없는, 조건에 맞는 브랜치 헤드마다 작업을 넣습니다.
- OwnGit은 시작할 때마다 작업을 한 번도 받지 못한, 조건에 맞는 브랜치 헤드에 작업을 넣습니다.
- 작업은 `pending`, `claimed`, `started`를 거쳐 `passed`, `failed`, `error`, `cancelled`, `incomplete`, `unavailable`, `ambiguous`, `interrupted` 중 하나로 끝납니다.
- 한 번 시작한 작업은 명령이 이미 실행되었을 수 있으므로 저절로 다시 대기열에 들어가지 않습니다. 직접 다시 실행하세요.
- 추적 중인 파일을 바꾼 체크는 깨끗한 결과를 받지 못합니다.

작업은 자동 체크 화면이나 명령줄에서 다룹니다.

```sh
owngit check-job list   --server https://git.example.test --repository project --password-file ./admin-password
owngit check-job show   ... --job JOB_ID
owngit check-job log    ... --job JOB_ID
owngit check-job cancel ... --job JOB_ID
owngit check-job rerun  ... --job JOB_ID
```

`...`는 같은 세 플래그입니다. `list`는 최근 작업 100개를 보여 줍니다. 다시 실행하면 같은 커밋을 같은 명령으로 체크합니다. 한도는 지금 정책의 최댓값을 따릅니다. 원본 로그 보관 기간은 [체크 원본 로그](#체크-원본-로그)를 보세요.

## 체크가 보는 파일

작업을 실행하기 전에 OwnGit은 바로 그 커밋의 파일을 새 비공개 폴더로 복사합니다.

- `.git` 폴더가 없으므로 Git 기록이 필요한 명령은 동작하지 않습니다.
- Git 속성, 필터, 훅, 줄바꿈 변환은 적용하지 않습니다.
- 심볼릭 링크와 서브모듈은 거부합니다. 이것을 추적하는 저장소는 자동 체크를 쓸 수 없습니다.
- Git LFS 파일은 포인터 파일 그대로 들어옵니다.
- 메모리가 적은 Linux 컴퓨터에서는 Git 프로세스 하나가 읽을 수 있는 크기보다 큰 파일을 거부합니다. 메모리가 512 MiB이면 약 16 MiB, 1 GiB이면 약 32 MiB입니다. 이 한도는 메모리에 따라 커지며 가장 커도 512 MiB입니다. 저장된 델타에서 다시 만드는 데 이 컴퓨터가 허용하는 것보다 메모리가 더 드는 파일도 거부합니다([메모리가 작은 Linux 호스트](OPERATIONS.ko.md#메모리가-작은-linux-호스트) 참고). 이때 작업은 실행되지 않고 끝납니다.

복사할 범위는 정책의 `execution.source`가 정합니다. 비워 두면 기본값을 씁니다.

| 필드 | 기본값 |
| --- | --- |
| `max_entries` | 트리 항목 20000개 |
| `max_file_bytes` | 파일 하나에 64 MiB |
| `max_total_bytes` | 모두 합쳐 256 MiB |
| `max_path_depth` | 폴더 깊이 64 |
| `max_path_bytes` | 경로 하나에 1024바이트 |
| `max_name_bytes` | 이름 하나에 255바이트 |
| `metadata_limit_bytes` | 트리 목록 16 MiB |

## 체크 원본 로그

서버는 작업이든 코딩 도구의 실행이든 체크마다 원본 로그(최대 256 KiB)를 30일 동안 보관합니다. 이 로그는 `owngit check-job log`로 볼 수 있습니다. 체크가 로그에 담을 수 있는 것보다 많이 출력하면 로그에는 출력의 앞부분과 끝부분이 남고, 그 사이에 빠진 바이트 수를 알려 주는 한 줄이 들어갑니다. 그래서 마지막 줄들도 보입니다. 작업 페이지는 로그를 최대 64 KiB까지 같은 방식으로 줄여서 보여 주며, 이때 그 한 줄은 보관된 로그에서 빠진 바이트 수를 셉니다. 출력 한도에 걸려 멈춘 체크는 그 뒤의 출력이 없습니다. 보관 기간을 바꾸려면 설정의 보관과 복구 탭에서 체크 원본 로그를 고르거나 다음을 실행하세요.

```sh
owngit settings set --check-logs 90d
```

고를 수 있는 값은 `7d`, `30d`, `90d`, `365d`, `indefinite`입니다. 기간을 줄이면 이미 보관 중인 로그에도 바로 적용됩니다. 로그가 만료되어도 결과는 남습니다.

## 백업과 복원

백업에는 정책, 작업, 결과가 들어가지만 러너 토큰은 들어가지 않습니다. 복원한 뒤에는 이렇게 됩니다.

- 다시 켤 때까지 체크가 꺼져 있습니다.
- 러너 토큰을 다시 발급해야 합니다.
- 끝나지 않았던 작업은 `interrupted`로 표시됩니다.
