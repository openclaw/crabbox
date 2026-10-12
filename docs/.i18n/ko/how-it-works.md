<!-- source: how-it-works.md; sha256: 2e0288463ff74a878d06a6226e5924a2ce5426c54796c463626ae14219ab37b5 -->
# Crabbox 작동 방식 {#how-crabbox-works}

처음 사용하는 사람, 전체 실행 모델이 필요한 사람, 코드를 바꾸기 전에 책임 범위를 확인하려는 사람을 위한 설명입니다.

## 요약 {#tl-dr}

Crabbox는 에이전트와 사람에게 주문형 머신을 제공합니다. 로컬에서 편집하면서 Linux, macOS, 네이티브 Windows, WSL2에서 테스트와 빌드를 실행합니다. 공유 환경의 두 축은 다음과 같습니다.

- **CLI**(`cmd/crabbox`, `internal/cli`): 임대, 변경된 작업 사본 동기화, 실행, 출력, 정리.
- **조정자**(`worker/src`): 공급자 자격 증명, 임대 상태, 만료, 정리, 사용량, 비용 제한. Cloudflare Workers/Durable Object 또는 Node.js/PostgreSQL/pg-boss에서 실행합니다.

관리형 실행기는 중개자 비밀을 보관하지 않습니다. 설정, 의존성, 워크로드는 저장소 책임입니다. 조정자는 기록, 로그, 원격 측정을 보존합니다. 스크린샷 등 아티팩트는 해제 전에 내려받거나 게시하세요. 직접 공급자는 조정자 없이도 실행합니다.

## 구성 요소 {#the-pieces}

```text
┌──────────────────────┐   HTTPS / JSON   ┌───────────────────────────┐
│ your machine         │ ───────────────► │ coordinator runtime       │
│ crabbox CLI          │  bearer + owner  │ Cloudflare + Durable Obj. │
│ repo checkout        │                  │ or Node.js + PostgreSQL   │
│ per-lease SSH key    │                  │ provider creds + state    │
└──────────┬───────────┘                  └────────────┬──────────────┘
           │                                         │ provider API
           │                                         ▼
           │                          ┌──────────────────────────────┐
           │ SSH (primary + fallback) │ managed cloud or sandbox     │
           └────────── rsync ────────►│ runner host                   │
                                      │ /work/crabbox/<lease>/<repo> │
                                      └──────────────────────────────┘
```

일반 CLI는 HTTPS로 조정자와 통신한 뒤 SSH/rsync로 실행기에 **직접** 연결합니다. 실행기는 일반 명령 실행을 위해 조정자를 호출하지 않으며 그 경로는 단방향입니다. 조정자는 작업 사본 전송이나 명령 실행 대신 임대를 관리합니다. CLI는 메타데이터, 출력 미리 보기, 로그, 결과를 별도 업로드하며 포털 브리지 트래픽도 조정자를 거칩니다. [사설 AWS 작업 공간](features/aws-private-workspaces.md)은 SSH 데이터 평면 없는 별도의 SSM-only API 경로입니다.

장기 상호작용은 인증된 `/v1/control` WebSocket으로 이벤트와 하트비트를 보내 높은 지연에서 폴링을 줄입니다. HTTPS는 정식 저장 및 호환성 대체 경로이므로 구버전도 상호 운용됩니다.

## 책임 {#ownership}

| 계층 | 소유하는 동작 |
| :--- | :--- |
| **CLI** | 설정/플래그, 임대별 SSH 키, 준비 확인, Git/rsync, 지문/무결성 검사, 명령과 스트림, WebSocket 및 HTTP 대체, 해제 |
| **Broker** | 인증/신원, 직렬화 상태, 자격 증명, 생성/삭제, 만료, 조회, 기록/로그/이벤트/원격 측정, 사용량/지출 한도 |
| **Provider** | 원시 컴퓨팅 자원. 어댑터가 Hetzner, AWS EC2, Azure VM, GCP 등을 생성/삭제 |
| **Runner** | 영속 책임 없음. Linux SSH/Git/rsync/작업 루트 준비, 다른 대상별 부트스트랩, 기존 정적 SSH 호스트. 프로젝트 런타임은 저장소가 설치 |

## `crabbox run`의 단계 {#what-crabbox-run-does}

일반 중개 SSH 실행은 다섯 단계입니다. 직접 공급자는 로컬 수명 주기, 위임 공급자는 실행 전송을 관리합니다.

**1. 계획.** 플래그 → 환경 → 저장소 → 사용자 → 기본값 순서로 설정합니다. `cbx_`와 12자리 16진수 ID 및 `<user-config>/crabbox/testboxes/<lease>/id_ed25519`의 키를 생성합니다. AWS/Azure Windows는 RSA입니다.

**2. 임대.** `POST /v1/leases`로 등급, 공급자, 대상, TTL, 유휴 제한, slug, 기능, 공개 키를 보냅니다. 조정자는 인증, 변경 직렬화, 활성/월간 제한, 실시간 가격, 최악 TTL 비용 예약, 리전/시장 대체를 수행하고 호스트, 사용자/포트, 작업 루트, 만료, 최종 ID와 slug를 돌려줍니다. ID가 바뀌면 키 디렉터리도 변경합니다.

**3. 동기화.** SSH와 준비 검사를 기다리고 origin/기준 ref에서 Git을 준비합니다. 지문이 같으면 생략합니다. 아니면 POSIX는 rsync(Linux의 `/work/crabbox/<lease>/<repo>`), 네이티브 Windows는 manifest tar를 전송한 뒤 무결성 검사와 지원되는 기준 이력 준비를 합니다.

**4. 실행.** 백그라운드 하트비트와 함께 SSH 명령의 stdout/stderr를 전달하고 중개 시 단계별 이벤트를 기록합니다.

**5. 해제.** 새 임대는 `--keep`이 없으면 해제하고 명시적 `--id` 재사용은 유지합니다. `--stop-after`로 바꿀 수 있습니다. 공급자가 정리하고 예약 비용을 반환합니다. 새 비유지 머신이 SSH 준비 전에 실패하면 `crabbox run`은 한 번 교체합니다. 유지 또는 명시적 재사용에서는 명령을 중복하지 않습니다.

## 준비된 머신 재사용 {#warm-boxes-and-reuse}

`crabbox warmup`은 같은 생성 경로를 사용하되 명령 없이 보관합니다. slug나 ID로 재사용합니다.

```sh
crabbox warmup --profile project-check --slug blue-lobster
crabbox run --id blue-lobster -- pnpm test:changed
crabbox ssh --id blue-lobster
crabbox stop blue-lobster
```

임대마다 `cbx_…` ID와 slug가 있습니다. 자동 slug는 `blue-lobster-1f3a9c2b` 같은 8자리 지문을 포함합니다. `--slug`로 지정하고 `--id`에 둘 다 사용할 수 있습니다. 하트비트는 `lastTouchedAt`과 유휴 만료를 갱신합니다. 방치하면 지속 스케줄러가 해제합니다.

## 중개와 직접 공급자 {#brokered-vs-direct-provider}

일반 경로는 **중개**입니다.

```text
CLI -> coordinator (Cloudflare or Node/PostgreSQL) -> provider API
CLI -> runner over SSH/rsync
```

공유 인프라에 적합하며 로컬에 비밀을 두지 않고 정리, 사용량, 비용을 관리합니다. `hetzner`, `aws`, `azure`, `daytona`, `gcp`에서 `CRABBOX_COORDINATOR` 또는 `config set-broker` URL이 있을 때만 사용됩니다.

설정이 없으면 **직접** 경로입니다.

```text
CLI -> provider API
CLI -> runner over SSH/rsync
```

AWS SDK 체인이나 Hetzner 토큰 같은 로컬 자격 증명이 필요합니다. 중앙 이력과 중개 하트비트는 없습니다. 중개자 진단 및 샌드박스, 정적 SSH, 로컬 컨테이너 등 직접 전용 어댑터에 사용합니다.

`broker.mode: registered`는 직접 경로를 유지하며 소유자별 메타데이터와 하트비트를 조정자에 복제합니다. 포털 목록, 선택적 공유, 아웃바운드 WebVNC를 추가하지만 자격 증명이나 정리 권한은 이전하지 않습니다. 해제는 기본으로 메타데이터만 제거합니다. 명시적 결합 어댑터는 사용자 확인 후 작업 공간을 삭제할 수 있습니다. 등록 기록은 관리형 할당량, 비용, 이미지, 풀, 유지 관리에 포함되지 않습니다.

정적 SSH(`provider: ssh`)는 중개 URL이 있어도 우회합니다. macOS/WSL2는 POSIX rsync, 네이티브 Windows는 PowerShell/tar를 사용합니다.

## 인증과 신원 {#auth-and-identity}

일반 사용자는 서명된 GitHub 토큰, 신뢰 자동화는 공유 bearer 토큰입니다. Cloudflare Access를 추가할 수 있으며 Node는 지정된 역방향 프록시 CIDR의 신원 헤더만 신뢰합니다.

```text
Authorization: Bearer <token>
X-Crabbox-Owner: <email>
X-Crabbox-Org: <org>
```

`crabbox login`의 소유자는 불변 `github:<numeric-id>`입니다. 공유 모드는 `CRABBOX_OWNER`, Git 이메일 환경, `git config user.email`을 사용하고 `CRABBOX_ORG`로 조직을 정합니다. 원시 Access 헤더는 무시하고 검증된 JWT 이메일만 인정합니다. [보안](security.md)을 보세요.

## 동기화 모델 {#sync-model}

깨끗한 작업 사본이 필요 없는 로컬 우선 모델입니다.

- origin/기준 ref의 Git 준비;
- rsync로 변경 파일 덮기와 선택적 체크섬;
- 지문으로 무변경 동기화 생략;
- 큰 디렉터리 제외;
- 추적 파일 대량 삭제 방지;
- 변경 테스트용 기준 이력 준비.

사람과 에이전트 모두 로컬 편집, 원격 실행입니다. `crabbox sync-plan`으로 읽기 전용 미리 보기가 가능합니다.

## 비용과 사용량 {#cost-and-usage}

임대별 두 가지 금액을 기록합니다.

```text
estimatedUSD   elapsed runtime cost so far
reservedUSD    worst-case TTL cost reserved before provisioning
```

시간당 가격의 우선순위:

```text
1. CRABBOX_COST_RATES_JSON   explicit override
2. provider live pricing     e.g. EC2 spot history, Hetzner server-type prices
3. built-in fallback rates   last-resort defaults
```

Hetzner 환율은 `CRABBOX_EUR_TO_USD`입니다. `CRABBOX_MAX_*`는 전역/소유자/조직별 활성 수와 월간 달러 한도이며 초과 생성은 HTTP 429입니다. `crabbox usage`는 `GET /v1/usage`를 조회하여 사용자, 조직, 공급자, 유형별로 묶습니다.

## 실패와 정리 {#failure-and-cleanup}

충돌, SSH 단절, cloud-init 실패, 부분 성공, 재시도, 로컬 프로세스보다 오래 남는 머신을 전제로 합니다.

- 결정 직렬화;
- 가능한 멱등 생성;
- Crabbox 태그;
- 반복해도 안전한 해제;
- Durable Object 알람 또는 pg-boss와 정기 조정;
- 보수적인 직접 정리;
- 중개자가 소유하는 중개 정리.

조정자 설정 시 `crabbox cleanup`은 자원 정리를 거부합니다. 그 책임은 조정자에 있습니다.

## 다음 문서 {#where-to-go-next}

- [아키텍처](architecture.md): 구성, API, 상태, 실패.
- [오케스트레이터](orchestrator.md): 책임, 수명, 정리, 비용.
- [CLI](cli.md): 명령, 설정, 출력, 코드.
- [기능](features/README.md): 기능별 문서.
- [명령](commands/README.md): 명령별 문서.
- [인프라](infrastructure.md): 런타임, 입구, 비밀, DNS, 설정.
- [보안](security.md): 인증, SSH, 정리, 신뢰 경계.
