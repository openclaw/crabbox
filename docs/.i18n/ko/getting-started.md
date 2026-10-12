<!-- source: getting-started.md; sha256: 6f010425dbbffe15196dfbc9cbfce66ea14783090c7babf231211bf13239fc09 -->
# 시작 가이드 {#getting-started}

Crabbox는 에이전트와 사람에게 주문형 컴퓨터를 제공합니다. 로컬에서 계속 편집하면 머신 임대, 커밋하지 않은 변경의 동기화, 명령 실행, 출력 스트리밍, 종료 코드 반환을 담당합니다. 일회성 실행은 머신을 해제하고 준비된 임대는 다음 실행까지 유지합니다.

원격 테스트와 빌드는 노트북의 CPU, 메모리, 포트를 비워 여러 에이전트의 병렬 작업을 지원합니다. Linux, macOS, 네이티브 Windows, WSL2를 지원하는 공급자를 선택하세요. 화면 작업은 [데스크톱, VNC, 브라우저, Code 및 포털 가이드](features/interactive-desktop-vnc.md)를 참고하세요.

신뢰하는 저장소에서 설치부터 `crabbox run -- pnpm test`까지 진행합니다. 먼저 설정과 설치 명령을 검토하세요. 계정 없는 첫 실행은 `--provider local-container`로 로컬 Docker나 Podman에서 가능합니다. [`crabbox-quickstart` 스킬](integrations/agents.md#install-through-ecosystem-skill-managers)이 전체 과정을 안내합니다.

## 1단계: 설치 {#step-1-install}

```sh
brew install openclaw/tap/crabbox
```

설치를 확인하세요.

```sh
crabbox --version
crabbox doctor
```

`crabbox doctor`는 검사마다 한 줄을 출력합니다. 새 설치의 공급자 중립적 `git` 검사는 `ok`여야 하며 공급자 도구 검사는 선택 후 시작됩니다. 미선택 시 `no provider selected`를 표시하고 `source=compiled_default selected=false`를 유지하며 자격 증명 준비 검사를 건너뛰되 실패하지 않습니다. 수명 주기 명령 전에 설정, `CRABBOX_PROVIDER`, `--provider`로 선택하세요. 선택 후 검사는 엄격하므로 자격 증명 구성 전에는 실패할 수 있습니다. `crabbox providers recommend`로 비교하세요.

Homebrew를 쓰지 않으면 <https://github.com/openclaw/crabbox/releases>에서 macOS, Linux, Windows 아카이브를 받으세요. macOS 실행 파일은 서명과 공증을 거쳤습니다.

포함된 `crabbox-runtime` 디렉터리는 실제 CLI 실행 파일 옆에 두세요. 두 Linux 아키텍처의 런타임은 일치하는 컨트롤러와 한 묶음입니다. CLI만 복사하거나 릴리스를 섞으면 완전한 런타임 팩이 아닙니다.

Go 사용자는 명시적 릴리스의 CLI만 설치할 수 있습니다. [GitHub Releases](https://github.com/openclaw/crabbox/releases)에서 태그를 고르세요. v0.44.0부터 지원합니다.

```sh
go install github.com/openclaw/crabbox/cmd/crabbox@v0.70.0
```

Go 1.26이 필요하며 권장 도구 체인은 go1.26.5입니다. Go 1.26.5 이상이나 자동 선택을 사용하세요. 이 명령은 CLI만 로컬 컴파일하며 `crabbox-apple-vm-helper`, 런타임 팩 등 부속 실행 파일과 자산을 설치하지 않고 서명·공증된 배포판도 아닙니다. 특히 Apple Silicon의 Apple VM 지원에는 Homebrew나 전체 아카이브를 사용하세요.

WSL2 및 x64 비 WSL Windows 전송 선택에는 현재 `main` 빌드나 Crabbox v0.42.1 이상이 필요합니다. [Windows 설치](windows-install.md)를 참고하세요.

## 2단계: 로그인 {#step-2-log-in}

공유 머신 풀에는 팀 조정자 URL을 사용하세요. 공급자 자격 증명, 임대 제한, 지출 한도, 만료를 관리합니다. 직접 클라우드와 로컬 공급자는 로그인하지 않습니다. [접근 경로 선택](#choosing-an-access-path)을 보세요.

```sh
crabbox login --url https://broker.example.com
```

`login`은 브라우저에서 GitHub OAuth를 엽니다. `--no-browser`는 같은 장치의 브라우저용 URL을 출력합니다. 중개자는 OAuth 코드를 교환하고 조직 멤버십을 확인한 뒤 일회용 확인을 CLI 루프백 리스너로 돌려보내고 서명 토큰을 사용자 설정에 저장합니다.

```text
logged in broker=https://broker.example.com provider=hetzner user=github:12345 org=example-org config=/Users/alice/Library/Application Support/crabbox/config.yaml
```

이후 `crabbox` 명령은 자동 인증됩니다. 현재 신원을 확인하세요.

```sh
crabbox whoami
```

```text
user=github:12345 org=example-org auth=user broker=https://broker.example.com
```

### 접근 경로 선택 {#choosing-an-access-path}

중개자 접근은 배포마다 다릅니다. 팀의 URL과 GitHub 조직/팀 허용 목록을 사용하세요. OAuth를 마쳐도 목록 밖 계정은 거부될 수 있습니다.

개인 또는 외부 설치에서는 다음 중 선택하세요.

- **직접 공급자:** 로컬 클라우드 자격 증명으로 빠른 개인 테스트를 수행합니다. 중앙 사용 이력과 공동 지출 한도 대신 로컬 정리와 상태를 수용합니다.
- **자체 조정자:** Cloudflare Workers 또는 Node.js/PostgreSQL에서 자격 증명, 활성 임대 제한, 월간 한도, `crabbox usage`, 지속적 정리, 팀 엔드포인트를 관리합니다.
- **접근 요청:** 운영자가 조직의 온보딩 절차를 제공할 때만 요청하세요. 팀 엔드포인트가 공개 중개자는 아닙니다.

직접 모드는 `login`을 건너뜁니다.

```sh
crabbox doctor --provider hetzner
crabbox run --provider hetzner -- pnpm test
```

자체 호스팅은 런타임 선택부터 시작합니다.

- **Cloudflare Workers:** Durable Object 상태, 알람, 예약 정리, 선택적 Cloudflare Access.
- **Node.js/PostgreSQL:** HTTP/WebSocket 서비스, PostgreSQL 13+ 상태, pg-boss 유지 관리 작업, 상시 단일 복제본. 초기 이식형 런타임으로 취급하고 운영 전환 전에 배포 검증을 완료하세요.

둘 다 동일한 비밀 정보, 인증 설정, 예산, API, 포털을 사용합니다. [인프라](infrastructure.md#choose-a-coordinator-runtime)를 보세요. 브라우저 로그인에는 GitHub OAuth 앱과 하나 이상의 허용 조직/팀이 필요하지만 공유 토큰 자동화에는 필요하지 않습니다.

브라우저를 열 수 없는 CI에서는 공유 토큰 인증을 사용하세요.

```sh
printf '%s' "$TOKEN" | crabbox login \
  --url https://broker.example.com \
  --provider aws \
  --token-stdin
```

전체 신원 모델은 [인증과 관리](features/auth-admin.md)를 보세요.

## 3단계: 저장소 준비 {#step-3-onboard-a-repo}

저장소에서 실행하세요.

```sh
crabbox init
```

`init`은 파일 세 개를 씁니다. `--config`, `--workflow`, 하나 이상의 `--skill`로 경로를 바꾸고 `--force`로 기존 파일을 덮어씁니다.

```text
.crabbox.yaml                          repo defaults (profile, class, sync, env)
.github/workflows/crabbox.yml          Actions hydration workflow (optional)
.agents/skills/crabbox/SKILL.md        agent-facing skill instructions
```

생성된 `.crabbox.yaml`을 저장소에 맞게 조정하세요.

- `profile`: 작업 이름. 템플릿은 `<repo>-check`.
- `class`: `tiny`, `small`, `standard`, `fast`, `large`, `beast`. 템플릿은 `beast`.
- `sync.exclude`: 전송하지 않을 디렉터리.
- `sync.include`: 루트 상대 허용 목록. 제외 적용 후 지정 경로**만** 동기화합니다.
- `env.allow`: 원격 명령에 허용할 환경 변수.

`--detect`는 테스트를 찾아 `jobs.detected`를 작성합니다. `crabbox job run detected`로 실행할 수 있습니다.

동기화 대상을 미리 확인하세요.

```sh
crabbox sync-plan
```

`sync-plan`은 파일 수, 총 바이트, 큰 파일을 표시합니다. 예상치 못한 `dist/`, `.cache/`, 대형 자산이 있으면 `sync.exclude`를 좁혀 다시 확인하세요. 최초 동기화 시간은 이 크기에 좌우됩니다.

`jj`가 설치된 네이티브 Git-backed Jujutsu 작업 공간도 지원합니다. 작업 사본 변경과 다중 부모 병합을 포함한 정확한 `@` 트리를 동기화합니다. `sync-plan --json`은 선택한 리비전을 표시하며 일반 SSH 공급자는 `run --no-hydrate`를 사용합니다. 함께 위치한 Git/Jujutsu는 Git 경로입니다. 조건과 안전 거부 규칙은 [Jujutsu 동기화](features/sync.md#jujutsu-workspaces)를 보세요.

## 4단계: 머신 준비 {#step-4-warm-a-box}

```sh
crabbox warmup --class standard --slug first-tests
```

`warmup`은 임대, 프로비저닝, SSH와 도구 대기 후 임대를 유지합니다. `--keep`은 기본으로 켜져 있으며 요약, 준비된 엔드포인트, 완료 시간을 출력합니다.

```text
leased cbx_abcdef123456 slug=first-tests provider=hetzner server=... type=... ip=203.0.113.10 idle_timeout=30m0s expires=...
ready ssh=crabbox@203.0.113.10:2222 network=public workroot=/work/crabbox
warmup complete total=...
```

조정자는 머신을 준비 상태로 유지합니다. 기본 유휴 제한 30분과 TTL 90분이 수명을 제한합니다. 직접 공급자는 자체 만료 정책을 사용하므로 완료 후 명시적으로 중지하세요. 크기 선택 전 `crabbox providers --json`으로 공급자별 등급을 확인하세요.

`slug` 또는 `id`(`cbx_...`)로 재사용합니다. 둘 다 이후 명령의 `--id`에 사용할 수 있습니다.

## 5단계: 명령 실행 {#step-5-run-a-command}

```sh
crabbox run --id first-tests -- pnpm test
```

진행 과정:

1. CLI가 SSH 준비 상태를 확인합니다.
2. origin과 기준 ref에서 원격 Git을 준비하고 변경된 트리를 rsync로 덮습니다. 지문이 같으면 동기화를 생략합니다.
3. SSH로 실행하고 stdout과 stderr를 스트리밍합니다.
4. 실행 중 유휴 만료를 막도록 중개자에 하트비트를 보냅니다.
5. 동기화 시간, 명령 시간, 종료 코드, Linux의 제한된 원격 측정을 `run_...` 이력에 기록합니다.

도구와 의존성 설치는 저장소 책임입니다. 필요하면 테스트 전에 설치 스크립트나 [Actions 환경 준비](features/actions-hydration.md)를 실행하세요.

일회성 실행은 `--id`를 생략합니다.

```sh
crabbox run -- pnpm test
```

새 임대를 얻어 실행하고 종료 시 해제합니다. 임시 테스트에는 일회성 실행, 반복 작업에는 `warmup` + `--id`를 사용하세요.

## 6단계: 이력 확인 {#step-6-inspect-history}

```sh
crabbox history
crabbox events run_abcdef123456
crabbox logs run_abcdef123456
crabbox results run_abcdef123456
```

`history`는 최근 실행, `events`는 순서 있는 이벤트(임대, 동기화, 명령, 출력, 완료), `logs`는 보존된 출력, `results`는 첨부된 JUnit 보고서를 표시합니다.

포털이 켜져 있으면 `/portal/runs/run_abcdef123456`에서 같은 내용을 봅니다.

## 7단계: 임대 중지 {#step-7-stop-the-lease}

완료한 뒤 실행하세요.

```sh
crabbox stop first-tests
```

`stop`은 공급자 계약에 따라 임대를 해제하고 예약 비용을 반환합니다. 대부분의 일회용 머신은 삭제되지만 정적 SSH 호스트와 유지 자원은 해당 정책을 따릅니다. 소유권 검증이 필요하며 복구 claim이나 고정 ID 종결 영수증은 로컬에 남을 수 있습니다. 잊은 중개 머신은 조정자가 만료 정리합니다.

```sh
crabbox cleanup --dry-run
```

`cleanup`은 직접 공급자의 잔여 자원과 로컬 상태용입니다. 중개 자원은 중개자의 알람이 정리합니다.

## 일반적인 변형 {#common-variations}

긴 세션 동안 유지하기:

```sh
crabbox warmup --idle-timeout 4h --ttl 8h --slug swift-crab
crabbox run --id swift-crab -- pnpm test
crabbox run --id swift-crab -- pnpm bench
crabbox stop swift-crab
```

데스크톱 열기:

```sh
crabbox warmup --desktop --browser --slug ui-check
crabbox webvnc --id ui-check --open --take-control
```

code-server 탭 열기:

```sh
crabbox warmup --code --slug code-check
crabbox run --id code-check --sync-only
crabbox code --id code-check --open
```

브라우저를 쓰는 동안 브리지 명령을 유지하세요. Desktop과 Code 지원은 대상과 공급자에 달려 있고 브라우저 Code에는 조정자와 격리된 Code origin도 필요합니다. [통합 접근 가이드](features/interactive-desktop-vnc.md)는 VNC, WebVNC, 포털, 공유, 정리를 설명합니다.

Zed Remote Projects에서 동기화한 작업 사본 열기:

```sh
crabbox run --id swift-crab --sync-only
crabbox open --editor=zed --id swift-crab
```

출력한 SSH 명령을 Zed의 **Connect New Server**에 붙여 넣고 표시된 폴더를 여세요. 작업 중 `crabbox open`을 유지하세요.

기존 Mac 사용하기(정적 SSH, 프로비저닝 없음):

```yaml
# .crabbox.yaml
provider: ssh
target: macos
static:
  host: mac-studio.local
  user: alice
  port: "22"
  workRoot: /Users/alice/crabbox
```

```sh
crabbox run -- xcodebuild test
```

명령마다 기본 공급자와 등급 재정의하기:

```sh
crabbox run --provider aws --class beast -- pnpm test
```

## 다음 단계 {#where-to-go-next}

- [Crabbox 작동 방식](how-it-works.md) — 기본 모델.
- [CLI](cli.md) — 명령과 종료 코드.
- [명령](commands/README.md) — 명령별 문서.
- [기능](features/README.md) — 기능별 문서.
- [설정](features/configuration.md) — YAML과 우선순위.
- [공급자](providers/README.md) — 실행 대상 선택.
- [공급자 작성](features/provider-authoring.md) — 공급자 추가.
- [문제 해결](troubleshooting.md) — 단계 실패 시 대응.
